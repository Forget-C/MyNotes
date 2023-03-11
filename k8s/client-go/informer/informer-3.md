# Informer（二）
**注意**：本文内容为学习笔记，内容为个人见解，不保证准确性，但欢迎大家讨论何指教。

本篇介绍`DeltaFIFO`及`indexer`。
informer大致工作流程如下：
![](../../../images/informer-2.png)
#### DeltaFIFO
`DeltaFIFO`是一个先进先出的队列，负责暂存监听的数据，后被`process`取出消费，用于中转数据。
```go
type DeltaFIFO struct {
	// 用于控制对items和queue的访问
	lock sync.RWMutex
	cond sync.Cond
	// 存放数据的map， map可以保证数据的唯一性
    // key由keyFunc生成， value为Deltas
	items map[string]Deltas
	// 存放items中的key，用于保证数据的顺序性
	queue []string
    // 用于生成key
	keyFunc KeyFunc
    // 省略部分代码...
}
type Delta struct {
    // 数据的类型，包括：Added, Updated等
	Type   DeltaType
    // 数据
	Object interface{}
}
const (
	Added   DeltaType = "Added"
	Updated DeltaType = "Updated"
	Deleted DeltaType = "Deleted"
	// Replaced is emitted when we encountered watch errors and had to do a
	// relist. We don't know if the replaced object has changed.
	//
	// NOTE: Previous versions of DeltaFIFO would use Sync for Replace events
	// as well. Hence, Replaced is only emitted when the option
	// EmitDeltaTypeReplaced is true.
	Replaced DeltaType = "Replaced"
	// Sync is for synthetic events during a periodic resync.
	Sync DeltaType = "Sync"
)
```
`DeltaFIFO`中实现了`Queue`接口，包括`Add`、`Update`等方法。
这些方法, 最终都是交给`f.queueActionLocked()`处理。
```go
func (f *DeltaFIFO) Add(obj interface{}) error {
	f.lock.Lock()
	defer f.lock.Unlock()
	f.populated = true
	return f.queueActionLocked(Added, obj)
}
```
`queueActionLocked`方法会将数据存储到`items`中，同时将`key`存储到`queue`中。操作完成后通知阻塞的协程。
```go 
func (f *DeltaFIFO) queueActionLocked(actionType DeltaType, obj interface{}) error {
	id, err := f.KeyOf(obj)
	if err != nil {
		return KeyError{obj, err}
	}
	oldDeltas := f.items[id]
	newDeltas := append(oldDeltas, Delta{actionType, obj})
    // 判断是否为重复的事件。
    // 事件类型如果均为Delete的话，会保留一个信息最多的
	newDeltas = dedupDeltas(newDeltas)

	if len(newDeltas) > 0 {
		if _, exists := f.items[id]; !exists {
			f.queue = append(f.queue, id)
		}
		f.items[id] = newDeltas
        // 通知阻塞的协程，实际是Pop()方法
		f.cond.Broadcast()
	} else {
		// 省略部分代码...
	}
	return nil
}
```
从这里可以看出`DeltaFIFO`是一个先进先出的队列。
`isInInitialList`参数在之前的版本是没有的，但是在`1.18`版本中加入了。这个参数的作用是用于标识当前数据是否是第一次同步的数据。当你的事件方法不需要区分第一次同步的数据和后续的数据时，可以忽略这个参数。
```go
func (f *DeltaFIFO) hasSynced_locked() bool {
    // populated为true，表示已经同步过一次数据
    // initialPopulationCount代表第一次同步的数据量
    // 在informer的场景中，启动数据变化监听时会先执行一次list获取全量数据。
    // initialPopulationCount代表第一list获取的数据量。
	return f.populated && f.initialPopulationCount == 0
}
func (f *DeltaFIFO) Pop(process PopProcessFunc) (interface{}, error) {
	f.lock.Lock()
	defer f.lock.Unlock()
	for {
		for len(f.queue) == 0 {
			if f.closed {
				return nil, ErrFIFOClosed
			}
            // 没有数据则阻塞，等待通知
			f.cond.Wait()
		}
		isInInitialList := !f.hasSynced_locked()
        // 队首弹出数据
		id := f.queue[0]
		f.queue = f.queue[1:]
		depth := len(f.queue)
		if f.initialPopulationCount > 0 {
			f.initialPopulationCount--
		}
        // 获取数据
		item, ok := f.items[id]
		if !ok {
			// This should never happen
			klog.Errorf("Inconceivable! %q was in f.queue but not f.items; ignoring.", id)
            // 如果获取不到，会获取下一顺位的数据。
            // 这也是为什么要套在for循环的原因。虽然这种情况永远不会发生。
			continue
		}
        // 删除数据
		delete(f.items, id)
		
		if depth > 10 {
			// 一些性能日志的打印...
		}
        // 调用process处理数据
        // informer场景中，process就是上一篇中的 processDeltas
		err := process(item, isInInitialList)
		if e, ok := err.(ErrRequeue); ok {
            // 如果返回ErrRequeue，则重新加入队列
			f.addIfNotPresent(id, item)
			err = e.Err
		}
		return item, err
	}
}
```
#### Indexer（Store）
`Indexer`是一个存储数据的地方，可以通过`key`获取数据，也可以通过`index`获取数据。
它实现了`Store`接口，包括`Add`、`Update`、`Delete`等方法。
`DeltaFIFO`中的数据最终会被存储到`Indexer`中。

`cache`是一个实现了`Indexer`接口的结构体。它主要是代理了`cacheStorage`的方法。
```go
type cache struct {
	// 线程安全的数据存储
	cacheStorage ThreadSafeStore
	// 生成key的方法，一般来说和DeltaFIFO中的keyFunc一致
    // 生成的key用于存储和索引数据
	keyFunc KeyFunc
}
```
`cache`的方法这里不赘述，主要看`ThreadSafeStore`的实现。
`ThreadSafeStore`是一个接口，粗略的来说，它主要定义了两部分：数据和索引
```go
type ThreadSafeStore interface {
    // 数据操作
	Add(key string, obj interface{})
	Update(key string, obj interface{})
	Delete(key string)
	Get(key string) (item interface{}, exists bool)
	List() []interface{}
	ListKeys() []string
	Replace(map[string]interface{}, string)
    // 索引操作
	Index(indexName string, obj interface{}) ([]interface{}, error)
	IndexKeys(indexName, indexedValue string) ([]string, error)
	ListIndexFuncValues(name string) []string
	ByIndex(indexName, indexedValue string) ([]interface{}, error)
	GetIndexers() Indexers
	AddIndexers(newIndexers Indexers) error
    // 弃用
	Resync() error
```
`storeIndex`实现了索引操作相关的功能。
这里的逻辑比较简单，不在赘述代码，举一个使用的例子说明一下：

比如我需要根据`Pod`的`image`来获取`Pod`的列表，那么我需要实现一个`IndexFunc`，这个`IndexFunc`的作用是根据`Pod`的`image`生成一个`key`。生成的`image key`对应的`value`是`Pod`的`name`（`name`实际是由`cache.keyfunc`生成的）,存放在`storeIndex.indices`中。
(代码请看[example](/example/k8s/client-go/informer-index/main.go))

```go
type storeIndex struct {
	// 索引的名称和keyFunc的映射
    // 添加一个索引其实就是添加一个keyFunc
	indexers Indexers
	// keyFunc生成的key和数据标识的映射。这里是1对多的关系
    // 例如：index名称为image，keyFunc生成的key为"nginx:v1.0"，数据标识为"kube-system/nginx"，则indices["image"]["nginx:v1.0"] = ["kube-system/nginx"]
	indices Indices
}
```
`storeIndex`的Add、Update、Delete操作对应的都是`updateIndices`这个方法。
- 对于创建，必须仅提供newObj
- 对于更新，必须同时提供oldObj和newObj
- 对于删除，必须仅提供oldObj
```go
func (i *storeIndex) updateIndices(oldObj interface{}, newObj interface{}, key string) {
	var oldIndexValues, indexValues []string
	var err error
	// 遍历注册的索引
	for name, indexFunc := range i.indexers {
		if oldObj != nil {
			oldIndexValues, err = indexFunc(oldObj)
		} else {
			oldIndexValues = oldIndexValues[:0]
		}
		if err != nil {
			panic(fmt.Errorf("unable to calculate an index entry for key %q on index %q: %v", key, name, err))
		}

		if newObj != nil {
			indexValues, err = indexFunc(newObj)
		} else {
			indexValues = indexValues[:0]
		}
		if err != nil {
			panic(fmt.Errorf("unable to calculate an index entry for key %q on index %q: %v", key, name, err))
		}

		index := i.indices[name]
		if index == nil {
			index = Index{}
			i.indices[name] = index
		}

		if len(indexValues) == 1 && len(oldIndexValues) == 1 && indexValues[0] == oldIndexValues[0] {
			continue
		}
		// 找到对应的索引进行操作
		for _, value := range oldIndexValues {
			i.deleteKeyFromIndex(key, value, index)
		}
		for _, value := range indexValues {
			i.addKeyToIndex(key, value, index)
		}
	}
}
```
`threadSafeMap`实现了数据操作相关的功能。
本质上一个加锁的`map`。`map`的key为`cache.keyfunc`生成，value为数据本身。
```go
type threadSafeMap struct {
	lock  sync.RWMutex
	// 存储数据
	items map[string]interface{}

	// 存储索引
	index *storeIndex
}
```
以`update`为例，`update`方法会调用`updateIndices`方法，更新索引。
```go
func (c *threadSafeMap) Update(key string, obj interface{}) {
	c.lock.Lock()
	defer c.lock.Unlock()
	oldObject := c.items[key]
	c.items[key] = obj
	// 更新索引
	c.index.updateIndices(oldObject, obj, key)
}
```
使用对应的索引进行查找时，需要指定索引名称和搜索值。
```go
// 默认的索引不需要指定索引名称
// key是由cache.keyfunc生成的
// 如Get("kube-system/kube-proxy")
func (c *threadSafeMap) Get(key string) (item interface{}, exists bool) {
	c.lock.RLock()
	defer c.lock.RUnlock()
	item, exists = c.items[key]
	return item, exists
}
// 通过索引查找指定索引名称
// 如ByIndex("image", "nginx:v1.0")
func (c *threadSafeMap) ByIndex(indexName, indexedValue string) ([]interface{}, error) {
	c.lock.RLock()
	defer c.lock.RUnlock()

	set, err := c.index.getKeysByIndex(indexName, indexedValue)
	if err != nil {
		return nil, err
	}
	list := make([]interface{}, 0, set.Len())
	for key := range set {
		list = append(list, c.items[key])
	}

	return list, nil
}
```
ok，到这里，我们就知道了`informer`是如何实现索引的了。
我们再来复盘一下整体的流程：
![](../../../images/informer-global.png)
1. `informer`启动时，会调用`informer`的`Run`方法，`Run`方法会启动`informer`的`controller`，`controller`会启动`reflector`。
2. `reflector`会启动一个`ListAndWatch`的`goroutine`，将数据写入到`DeltaFIFO`中。
3. `controller`还有一个`processLoop`的`goroutine`，从`DeltaFIFO`中读取数据，将数据写入到`Store（indexer）`中，并触发`informer`的`EventHandler`。
4. `DeltaFIFO`是一个先进先出队列，使用`list`实现数据有序，使用`map`实现数据存储和去重。
5. `Store（indexer）`本质是一个加了锁的`map`。自定义索引时通过`IndexFunc`生成索引键。



