# Informer（二）

本篇介绍`cache.SharedIndexInforme`中 Controller及其组件
informer大致工作流程如下：
![](../../../images/informer-2.png)
#### sharedIndexInformer
根据上一篇使用 Deployment 资源类型的informer创建举例， 我们可以定位到最终实现为:tools/cache/shared_informer.go

结构体定义如下：
```go
type sharedIndexInformer struct {
	// 数据存储结构
	indexer    Indexer
	// 事件处理器
	controller Controller 
	// 用于分发事件到EventHandler
	processor             *sharedProcessor
	// 用于检查对象是否被篡改
	cacheMutationDetector MutationDetector 
	// 用于获取资源数据
	listerWatcher ListerWatcher
	// 当前监听的资源类型
	objectType runtime.Object
	// 资源在被分发前进行的处理操作
	transform TransformFunc
	// 省略部分代码...
}
```
informer启动函数：
```go
func (s *sharedIndexInformer) Run(stopCh <-chan struct{}) {
	// 初始化数据队列，用于资源数据的中转。
	fifo := NewDeltaFIFOWithOptions(DeltaFIFOOptions{
		KnownObjects:          s.indexer,
		EmitDeltaTypeReplaced: true,
	})

	cfg := &Config{
		Queue:             fifo,
		ListerWatcher:     s.listerWatcher,
		ObjectType:        s.objectType,
		ObjectDescription: s.objectDescription,
		FullResyncPeriod:  s.resyncCheckPeriod,
		RetryOnError:      false,
		ShouldResync:      s.processor.shouldResync,

		Process:           s.HandleDeltas,
		WatchErrorHandler: s.watchErrorHandler,
	}
    // 省略部分代码...
	// ...
	// 启动数据突变检测器
	wg.StartWithChannel(processorStopCh, s.cacheMutationDetector.Run)
    // 启动事件分发器
	wg.StartWithChannel(processorStopCh, s.processor.run)
    // 启动数据控制器
	s.controller.Run(stopCh)
}
```
#### SharedProcessor
`SharedProcessor`用于管理`processorListener`。
`processorListener`监听`OnAdd`,`OnUpdate`,`OnDelete`这些动作，交由对应的注册的事件函数处理。
`sharedProcessor`的代码这里就不赘述了，着重看一下`processorListener`的实现。
```go
type processorListener struct {
	// 事件首先会放到addCh中
	addCh  chan interface{}
	// addCh中的事件，会放到nextCh中，守护协程就可以读到事件了
	// 来不及处理的事件，会放到pendingNotifications这个环形队列中
    pendingNotifications buffer.RingGrowing
	// 事件最终会放到这个chan中
	nextCh chan interface{}
    // 注册的事件处理函数
	handler ResourceEventHandler
    // 省略部分代码...
}
```
pop函数用于将addCh中的事件取出,基本逻辑如下：
![](../../../images/infomer-processListen.png)
```go
func (p *processorListener) pop() {
	defer utilruntime.HandleCrash()
	defer close(p.nextCh) 

	var nextCh chan<- interface{}
	var notification interface{}
	for {
		select {
		// 将取出的事件放到nextCh中
		case nextCh <- notification:
			var ok bool
			// 读取缓冲区，缓冲区为空时将notification和nextCh置为nil
			// 这样就可以继续走下面的写入逻辑
			notification, ok = p.pendingNotifications.ReadOne()
			if !ok { // Nothing to pop
				nextCh = nil // Disable this select case
			}
		case notificationToAdd, ok := <-p.addCh:
			if !ok {
				return
			}
			if notification == nil { 
				// notification 为nil时，代表目前没有未处理的事件。
				// 这时候将nextCh，notification分别赋值，下一次循环就会将事件放到nextCh中
				notification = notificationToAdd
				nextCh = p.nextCh
			} else {
				// notification不为nil时，代表有未处理完的事件，将事件放到环形队列中
				p.pendingNotifications.WriteOne(notificationToAdd)
			}
		}
	}
}
```
run方法从nextCh中消费
```go
func (p *processorListener) run() {
	stopCh := make(chan struct{})
	wait.Until(func() {
		for next := range p.nextCh {
			switch notification := next.(type) {
			case updateNotification:
			    p.handler.OnUpdate(notification.oldObj, notification.newObj)
		    // 省略部分代码...
		}
		close(stopCh)
	}, 1*time.Second, stopCh)
}
```
#### controller
`controller`是`sharedIndexInformer`的核心。
它负责从`listerWatcher`获取资源数据，然后将数据存储到`indexer`中，同时将数据分发到`processor`中。
我们来看看`controller`的定义：
```go
type controller struct {
	config         Config
	// 用于获取数据，并转换成目标资源对象
	reflector      *Reflector
	reflectorMutex sync.RWMutex
	clock          clock.Clock
}
```
`controller`的启动函数：
```go
func (c *controller) Run(stopCh <-chan struct{}) {
	// 省略部分代码...
	r := NewReflectorWithOptions(
		c.config.ListerWatcher,
		c.config.ObjectType,
		c.config.Queue,
		ReflectorOptions{
			ResyncPeriod:    c.config.FullResyncPeriod,
			TypeDescription: c.config.ObjectDescription,
			Clock:           c.clock,
		},
	)
    // 省略部分代码...

	c.reflectorMutex.Lock()
	c.reflector = r
	c.reflectorMutex.Unlock()
	var wg wait.Group
	// 启动反射器，获取数据
	wg.StartWithChannel(stopCh, r.Run)
	// 启动processLoop，操作数据
	wait.Until(c.processLoop, time.Second, stopCh)
	wg.Wait()
}
```
`NewReflectorWithOptions`会根据配置创建对应的`Reflector`对象。
`Reflector.Run`会启动一个`ListAndWatch`的goroutine，用于获取资源数据。
`ListAndWatch`会携带最后一次的资源版本号，加上重试机制来保证不会丢失数据。
然后将数据存储到`DeltaFIFO`中。
```go
func (r *Reflector) Run(stopCh <-chan struct{}) {
    // 省略部分代码...
    wait.BackoffUntil(func() {
        if err := r.ListAndWatch(stopCh); err != nil {
        r.watchErrorHandler(r, err)
    }
    }, r.backoffManager, true, stopCh)
}
func (r *Reflector) ListAndWatch(stopCh <-chan struct{}) error {
	// 首先list获取全量数据 
	err := r.list(stopCh)
	if err != nil {
		return err
	}
    // 省略部分代码...
	// 请求APIServer的参数
	options := metav1.ListOptions{
		// 指定了资源版本，这样就可以获取到资源版本之后的数据
        ResourceVersion: r.LastSyncResourceVersion(),
        TimeoutSeconds:
        } &timeoutSeconds,
		AllowWatchBookmarks: true,
    }
    // 重试器，仅限于调用apiServer的网络错误，比如超时、连接中断等
	retry := NewRetryWithDeadline(r.MaxInternalErrorRetryDuration, time.Minute, apierrors.IsInternalError, r.clock)
	for {
        // 省略部分代码...
		// 数据操作函数
		err = watchHandler(start, w, r.store, r.expectedType, r.expectedGVK, r.name, r.typeDescription, r.setLastSyncResourceVersion, r.clock, resyncerrc, stopCh)
		retry.After(err)
		if err != nil {
			if err != errorStopRequested {
				switch {
                // 省略部分代码...
				case apierrors.IsTooManyRequests(err):
					<-r.initConnBackoffManager.Backoff().C()
					continue
			}
			return nil
		}
	}
}
```
watchHandler
```go
func watchHandler() error {
// 省略部分代码...
loop:
	for {
		select {
		case <-stopCh:
			return errorStopRequested
		case err := <-errc:
			return err
		case event, ok := <-w.ResultChan():
			if !ok {
				// 这里不清楚为什么要用这种方式
				// 可能是历史问题，这部分的代码比较老，是2016年的
				break loop
			}
			// 一堆数据合法性判断，省略部分代码...
			resourceVersion := meta.GetResourceVersion()
			switch event.Type {
			case watch.Added:
				// 写入到DeltaFIFO
				err := store.Add(event.Object)
				if err != nil {
					utilruntime.HandleError(fmt.Errorf("%s: unable to add watch event object (%#v) to store: %v", name, event.Object, err))
				}
			case watch.Modified:
				// 省略部分代码...
			case watch.Deleted:
				// 省略部分代码...
			case watch.Bookmark:
			default:
				utilruntime.HandleError(fmt.Errorf("%s: unable to understand watch event %#v", name, event))
			}
			// 更新记录的版本号，请求的时候会用到
			setLastSyncResourceVersion(resourceVersion)
			if rvu, ok := store.(ResourceVersionUpdater); ok {
				rvu.UpdateResourceVersion(resourceVersion)
			}
			eventCount++
		}
	}
	// 省略部分代码...
}
```
#### processLoop

`processLoop`会从`DeltaFIFO`中获取数据，然后将数据存储到`indexer`中，同时将数据分发到`processor`中。

```go
func (c *controller) processLoop() {
	for {
		// 函数是在config中传入的
		obj, err := c.config.Queue.Pop(PopProcessFunc(c.config.Process))
		// 省略部分代码...
	}
}
// 最终定位到的函数
func processDeltas(
	handler ResourceEventHandler,
	clientState Store, 
	transformer TransformFunc,
	deltas Deltas,
	isInInitialList bool,
) error {
	// 代码里面有多处用Store关键字的地方
	// 命名也有很多store
	// 不同的位置，代表的含义也不一样，这里需要注意
	for _, d := range deltas {
		obj := d.Object
		// transformer用于在处理数据之前对数据进行转换
		// 默认transformer是nil
		if transformer != nil {
			var err error
			obj, err = transformer(obj)
			if err != nil {
				return err
			}
		}

		switch d.Type {
		case Sync, Replaced, Added, Updated:
			// 省略部分代码...
			// 写入到indexer中，即最终的数据存储位置
			if err := clientState.Update(obj); err != nil {
					return err
				}
			// 执行事件处理函数
			handler.OnUpdate(old, obj)
		case Deleted:
			// 省略部分代码...
		}
	}
	return nil
}	
```

#### 总结
`informer`中`controller`负责数据控制，包括：获取数据、数据处理、数据分发。
`controller`中由：
- `Reflector`负责获取数据并写入到队列。
  数据获取时会携带上一次的版本号，这样就可以获取到版本号之后的数据。
- `processLoop`取出数据，写入到`indexer`中，同时将数据分发到`processor`中。`processor`中会根据动作类型，`OnAdd`、`OnUpdate`、`OnDelete`，执行对应的事件处理函数。
