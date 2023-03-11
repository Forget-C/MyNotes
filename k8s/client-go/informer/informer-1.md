# Informer(一)
**注意**：本文内容为学习笔记，内容为个人见解，不保证准确性，但欢迎大家讨论何指教。

本篇为先导篇， 介绍informer的入口工厂函数。
#### informer目录结构 (仅展示部分目录，省略的目录相似)
```bash
client-go|master⚡ ⇒ tree informers -L 2
informers
├── apps
│   ├── interface.go
│   ├── v1
│   ├── v1beta1
│   └── v1beta2
├── core
│   ├── interface.go
│   └── v1
├── doc.go
├── factory.go
├── flowcontrol
├── generic.go
├── node
│   ├── interface.go
│   ├── v1
│   ├── v1alpha1
│   └── v1beta1
└── storage
    ├── interface.go
    ├── v1
    ├── v1alpha1
    └── v1beta1

65 directories, 23 files
```
可以看到，`factory.go`为工厂函数的文件，作为调用的入口。每个资源类型为单独的文件夹， 按照版本号划分子文件夹。

#### factory
``` go
type sharedInformerFactory struct {
	client           kubernetes.Interface
	namespace        string
	tweakListOptions internalinterfaces.TweakListOptionsFunc
	lock             sync.Mutex
	defaultResync    time.Duration
	customResync     map[reflect.Type]time.Duration
   
   // 存放对应资源类型的informer
	informers map[reflect.Type]cache.SharedIndexInformer
   // informer启动状态
	startedInformers map[reflect.Type]bool
	// 用于等待多个资源类型的informer启动
	wg sync.WaitGroup
	
	shuttingDown bool
}
```
对应资源的监听实现，通过`InformerFor`方法传入并记录。
sharedInformer 将多种资源放在map中保存。
重复监听相同资源的动作是安全的。
``` go
func (f *sharedInformerFactory) InformerFor(obj runtime.Object, newFunc internalinterfaces.NewInformerFunc) cache.SharedIndexInformer {
	f.lock.Lock()
	defer f.lock.Unlock()

	informerType := reflect.TypeOf(obj)
	informer, exists := f.informers[informerType]
	// 如果资源已经监听过了，则什么都不做
	if exists {
		return informer
	}

	resyncPeriod, exists := f.customResync[informerType]
	if !exists {
		resyncPeriod = f.defaultResync
	}

	informer = newFunc(f.client, resyncPeriod)
	f.informers[informerType] = informer

	return informer
}
```
多次调用`Start()`是安全的
```go
func (f *sharedInformerFactory) Start(stopCh <-chan struct{}) {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.shuttingDown {
		return
	}

	for informerType, informer := range f.informers {
	   // 只会run新的资源类型
		if !f.startedInformers[informerType] {
			f.wg.Add(1)
			informer := informer
			go func() {
				defer f.wg.Done()
				informer.Run(stopCh)
			}()
			f.startedInformers[informerType] = true
		}
	}
}
```
调用对应资源方法，对应的实现在上面的资源目录
``` go
func (f *sharedInformerFactory) Internal() apiserverinternal.Interface {
	return apiserverinternal.New(f, f.namespace, f.tweakListOptions)
}

func (f *sharedInformerFactory) Apps() apps.Interface {
	return apps.New(f, f.namespace, f.tweakListOptions)
}
```
#### resource
以apps目录举例
```
informers
├── apps
│   ├── interface.go
│   ├── v1
│   ├── v1beta1
│   └── v1beta2
```
apps在当前的存在3个版本，故对应三个文件夹。
`interface.go`为当前资源入口。
```go
type group struct {
   // 传入的工厂对象
	factory          internalinterfaces.SharedInformerFactory
	namespace        string
	tweakListOptions internalinterfaces.TweakListOptionsFunc
}

// New returns a new Interface.
func New(f internalinterfaces.SharedInformerFactory, namespace string, tweakListOptions internalinterfaces.TweakListOptionsFunc) Interface {
   // 当调用factory.Apps()时，工厂对象是传入的，不会创建新的工厂，也就不会创建新的liste/watch连接。
   // SharedInformer中的shared就是指这个。
	return &group{factory: f, namespace: namespace, tweakListOptions: tweakListOptions}
}

// V1 returns a new v1.Interface.
func (g *group) V1() v1.Interface {
	return v1.New(g.factory, g.namespace, g.tweakListOptions)
}

// V1beta1 returns a new v1beta1.Interface.
func (g *group) V1beta1() v1beta1.Interface {
	return v1beta1.New(g.factory, g.namespace, g.tweakListOptions)
}

// V1beta2 returns a new v1beta2.Interface.
func (g *group) V1beta2() v1beta2.Interface {
	return v1beta2.New(g.factory, g.namespace, g.tweakListOptions)
}
```
当我们调用factory.Apps().V1().Deployments()，实现文件为：
```
informers
├── apps
│   ├── interface.go
│   ├── v1
        ├── deployment.go
```
调用factory.Apps().V1().Deployments().Informer()， 会触发工厂函数的`InformerFor()`方法监听资源。
重复：`InformerFor()`方法，重复监听相同资源的动作是安全的。
```go
func (f *deploymentInformer) Informer() cache.SharedIndexInformer {
	return f.factory.InformerFor(&appsv1.Deployment{}, f.defaultInformer)
}
```
任意资源有自己的监听函数的实现， Deployments的为：
``` go
func NewFilteredDeploymentInformer(client kubernetes.Interface, namespace string, resyncPeriod time.Duration, indexers cache.Indexers, tweakListOptions internalinterfaces.TweakListOptionsFunc) cache.SharedIndexInformer {
	return cache.NewSharedIndexInformer(
	   // 定义 list/watch规则。
	   // 实际上所有资源类型的informer， 最终都会走到cache.SharedIndexInforme。
	   // 根据不同的ListWatch对象决定监听不同的资源。 这是informer实现的基础。
	   // 这里 ListWatch 监听的是 AppsV1().Deployments
		&cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				if tweakListOptions != nil {
					tweakListOptions(&options)
				}
				return client.AppsV1().Deployments(namespace).List(context.TODO(), options)
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				if tweakListOptions != nil {
					tweakListOptions(&options)
				}
				return client.AppsV1().Deployments(namespace).Watch(context.TODO(), options)
			},
		},
		&appsv1.Deployment{},
		resyncPeriod,
		indexers,
	)
}
// 上面Informer()函数中传入的方法
func (f *deploymentInformer) defaultInformer(client kubernetes.Interface, resyncPeriod time.Duration) cache.SharedIndexInformer {
	return NewFilteredDeploymentInformer(client, f.namespace, resyncPeriod, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}, f.tweakListOptions)
}
```

#### 总结
![](../../../images/informer-1.png)
`informers/factory.go`为工厂方法实现文件。
“工厂”产出不同资源类型的informer。
资源通过`InformerFor`记录到“工厂”中，通过`Start`方法启动监听。这两个方法重复调用均为安全操作。
最终的数据处理由`cache.SharedIndexInforme`实现。


