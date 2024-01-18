# 从源码解析KubeScheduler Framework插件
本文从源码的角度分析KubeScheduler Framework相关功能的实现。 

本篇kubernetes版本为v1.27.3。

>kubernetes项目地址: [https://github.com/kubernetes/kubernetes](https://github.com/kubernetes/kubernetes)
>
>scheduler命令main入口: cmd/kube-scheduler/scheduler.go
>
>scheduler相关代码目录: pkg/scheduler
>
>scgeduler调度流程解析: [《从源码解析KubeScheduler调度过程》](/cloud_native/k8s/scheduler/scheduler_flow)

## Framework插件介绍
`Framework`是`KubeScheduler`的核心组件，它管理了资源分配和调度策略等相关插件。 这些插件联合起来就实现了`KubeScheduler`的调度功能。

`scheduler`在node列表中取出一个node，然后依次调用"参与调度的插件"的`PreFilter`、`Filter`等方法，如果所有插件都返回成功，则调度成功，否则调度失败。

插件运行大概分为以下几个阶段（也是对应的interface的名称）:
- PrePreEnqueuePlugin 在进入到调度队列之前运行的插件，他将判断pod是否可以被调度。
- PreFilterPlugin 过滤前的预处理, 根据pod中已知的信息，准备后续处理需要的数据。
- FilterPlugin 主要的过滤功能，如查看node资源是否足够、存储卷是否准备充分都是在这一阶段发生的。
- PostFilterPlugin 在Filter通过后进行调用。为了处理缺少运行资源的场景。这个插件目前只有`dynamicResources`一个实现。`dynamicResources`是一个alpha功能，用于处理`pod`和需要动态申请的资源之间的关联关系。
- PreScorePlugin 评分前的预处理
- ScorePlugin 评分插件, 对过滤后的节点进行打分。个别插件运行`Score`还需要运行`NormalizeScore`进行归一化， 将分数统一标准。
- ReservePlugin 这个插件会维护之前步骤产生的`状态数据`，这些状态数据是为资源保留。如果任何一个插件的`Reserve`调用失败，将会调用`Unreserve`执行方向的会滚操作。
- PermitPlugin 许可插件, `pod`在这里会被设置为 批准绑定(到目标节点)、拒绝绑定、延迟绑定。
- PreBindPlugin 绑定前的准备工作。
- BindPlugin 绑定工作。
- PostBindPlugin 绑定后的处理工作,如资源清理。

比如`PreFilterPlugin`定义如下:
```go
type Plugin interface {
	Name() string
}
type PreFilterPlugin interface {
	Plugin
	PreFilter(ctx context.Context, state *CycleState, p *v1.Pod) (*PreFilterResult, *Status)
	PreFilterExtensions() PreFilterExtensions
}
```
更多插件interface的定义可以查看文件： pkg/scheduler/framework/interface.go。

一个插件可以实现多个interface，比如`InterPodAffinity`（pod亲和性插件）就同时实现了`FilterPlugin`和`PreFilterPlugin`两个interface。

## 插件调用过程
![](scheduling-framework-extensions.png)
假如现在有 A、B、C 三个插件（假如只有这三个插件，仅为方便理解调用顺讯)
1. 插件的注册顺序为 A -> B -> C。
2. A实现了PreFilterPlugin, 和 FilterPlugin
3. B实现了FilterPlugin
4. C实现了PreFilterPlugin, 和 PostFilterPlugin
```mermaid
graph LR
    APre[A.PreFilterPlugin]
    AFilter[A.FilterPlugin]
    BFilter[B.FilterPlugin]
    CPre[C.PreFilterPlugin]
    CPost[C.PostFilterPlugin]
    APre --> CPre --> AFilter --> BFilter --> CPost
```
也就是说，scheduler在调度过程中，会依次调用插件的PreFilter、Filter、PostFilter方法, 插件的顺序是插件注册的顺序。

期间任何一个插件调用返回错误，都会导致调度终止。
## 内置插件
内置插件支持了`scheduler`的基本功能, 可以通过配置来控制指定插件的启用/停用。

下面挑选几个相对比较重要的插件进行分析。
## NodeName
先看一个简单的。`nodeName`是一个硬性标准，且计算所需要的信息已经存在于`pod`资源定义中（`nodeSelector`），所以只实现了`Filter`接口。
```go
func (pl *NodeName) Filter(ctx context.Context, _ *framework.CycleState, pod *v1.Pod, nodeInfo *framework.NodeInfo) *framework.Status {
	if nodeInfo.Node() == nil {
		return framework.NewStatus(framework.Error, "node not found")
	}
	if !Fits(pod, nodeInfo) {
		return framework.NewStatus(framework.UnschedulableAndUnresolvable, ErrReason)
	}
	return nil
}
func Fits(pod *v1.Pod, nodeInfo *framework.NodeInfo) bool {
    // 硬性标准，直接判断是否相等
	return len(pod.Spec.NodeName) == 0 || pod.Spec.NodeName == nodeInfo.Node().Name
}
```
## Fit
`Fit`也叫做"NodeResourcesFit", `Fit`用于检查node资源和pod申请资源。
### PreFilter
计算好pod resource中声明的需要的资源，写入state。如果pod支持资源伸缩的话， 还会计算pod的最大资源需求。
```go
func (f *Fit) PreFilter(ctx context.Context, cycleState *framework.CycleState, pod *v1.Pod) (*framework.PreFilterResult, *framework.Status) {
    // 仅将计算好的数据写入了state， 没有额外操作
	cycleState.Write(preFilterStateKey, computePodResourceRequest(pod))
	return nil, nil
}
```
### Filter
```go
func (f *Fit) Filter(ctx context.Context, cycleState *framework.CycleState, pod *v1.Pod, nodeInfo *framework.NodeInfo) *framework.Status {
    // 获取PreFilter计算的结果
	s, err := getPreFilterState(cycleState)
	if err != nil {
		return framework.AsStatus(err)
	}
    // 这里的计算代码比较简单， 就不展开了
    // 主要是检查几个资源
    // 1. node数量是否超过限制 2. node上的资源是否满足pod request的需求，自动伸缩资源的pod以扩展的最大上限为准
	insufficientResources := fitsRequest(s, nodeInfo, f.ignoredResources, f.ignoredResourceGroups)
    // 如果有不满足的资源， 则返回错误
	if len(insufficientResources) != 0 {
		// We will keep all failure reasons.
		failureReasons := make([]string, 0, len(insufficientResources))
		for i := range insufficientResources {
			failureReasons = append(failureReasons, insufficientResources[i].Reason)
		}
		return framework.NewStatus(framework.Unschedulable, failureReasons...)
	}
	return nil
}
```
### PreScore
如果`pod`为设置资源限制， 则会在`state`中填充一个默认值， 方便（只是为了计算，不会体现在`pod`声明中）后面的score计算。
```go
// 最终的填充函数
func (r *resourceAllocationScorer) calculatePodResourceRequest(pod *v1.Pod, resourceName v1.ResourceName) int64 {
    // 省略一些代码
    // 如果是 NonZeroRequested的容器， 则会设置一个默认值
	if !r.useRequested {
		opts.NonMissingContainerRequests = v1.ResourceList{
            // 0.1 core
			v1.ResourceCPU:    *resource.NewMilliQuantity(schedutil.DefaultMilliCPURequest, resource.DecimalSI),
            // 200 MB
			v1.ResourceMemory: *resource.NewQuantity(schedutil.DefaultMemoryRequest, resource.DecimalSI),
		}
	}
    // 省略一些代码
	return quantity.Value()
}
```
### Score
在源码中`Score`的计算方式有三种(不知道为什么官方文档中只有两种，打开姿势不对？):
- MostAllocated 优选分配比率较高的节点
- LeastAllocated 优选分配比率较低的节点
- RequestedToCapacityRatio 可以基于请求值与容量的比率，针对参与节点计分的每类资源设置权重。
下面是MostAllocated的计算函数：
```go
// MostAllocated 最终会由mostResourceScorer函数计算分数
// 外层的处理函数主要是负责取出目标node的已分配资源和总资源， 生成requested, allocable传入这个函数
// config.ResourceSpec存放的是每项资源的权重信息
// (cpu(MaxNodeScore * requested * cpuWeight / capacity) + memory(MaxNodeScore * requested * memoryWeight / capacity) + ...) / weightSum
func mostResourceScorer(resources []config.ResourceSpec) func(requested, allocable []int64) int64 {
	return func(requested, allocable []int64) int64 {
		var nodeScore, weightSum int64
		for i := range requested {
			if allocable[i] == 0 {
				continue
			}
			weight := resources[i].Weight
            // 计算函数
			resourceScore := mostRequestedScore(requested[i], allocable[i])
            // 带入权重计算
			nodeScore += resourceScore * weight
			weightSum += weight
		}
		if weightSum == 0 {
			return 0
		}
		return nodeScore / weightSum
	}
}
func mostRequestedScore(requested, capacity int64) int64 {
	if capacity == 0 {
		return 0
	}
	if requested > capacity {
		requested = capacity
	}
    // 使用率
	return (requested * framework.MaxNodeScore) / capacity
}
```
`LeastAllocated`的计算函数与MostAllocated的计算函数类似， 只是将计算函数替换为了`leastRequestedScore`
```go
// (cpu((capacity-requested)*MaxNodeScore*cpuWeight/capacity) + memory((capacity-requested)*MaxNodeScore*memoryWeight/capacity) + ...)/weightSum
func leastRequestedScore(requested, capacity int64) int64 {
	if capacity == 0 {
		return 0
	}
	if requested > capacity {
		return 0
	}
    // 空闲率
	return ((capacity - requested) * framework.MaxNodeScore) / capacity
}
```
`RequestedToCapacityRatio`允许自定义打分的标准， 所以比上面两个多了`shape`参数。先来看一下配置文件，方便理解。
```yaml
apiVersion: kubescheduler.config.k8s.io/v1beta3
kind: KubeSchedulerConfiguration
profiles:
- pluginConfig:
  - args:
      scoringStrategy:
        resources:
        ## 权重的定义和mostAllocated一样
        - name: intel.com/foo
          weight: 3
        - name: intel.com/bar
          weight: 3
        requestedToCapacityRatio:
        ## 这一块是定义打标准的
          shape:
          ## 利用率是0的时候，分数是0
          - utilization: 0
            score: 0
          ## 利用率是100的时候，分数是10
          - utilization: 100
            score: 10
          ## 这样的评分标准是mostAllocated， 即优先向利用率高的节点调度
          ## 如果想启用最少请求（least requested）则反转评分标准
            #   - utilization: 0
            #     score: 10
            #   - utilization: 100
            #     score: 0
        type: RequestedToCapacityRatio
    name: NodeResourcesFit
```
再来看代码实现:
```go
func requestedToCapacityRatioScorer(resources []config.ResourceSpec, shape []config.UtilizationShapePoint) func([]int64, []int64) int64 {
	shapes := make([]helper.FunctionShapePoint, 0, len(shape))
	for _, point := range shape {
		shapes = append(shapes, helper.FunctionShapePoint{
			Utilization: int64(point.Utilization),
			// MaxCustomPriorityScore是指插件的满分是多少, 这里是10
            // MaxNodeScore是当前节点的满分是多少， 这里是100
            // 这里的计算操作会把分数缩放到允许的分数范围内
			Score: int64(point.Score) * (framework.MaxNodeScore / config.MaxCustomPriorityScore),
		})
	}
	return buildRequestedToCapacityRatioScorerFunction(shapes, resources)
}
func buildRequestedToCapacityRatioScorerFunction(scoringFunctionShape helper.FunctionShape, resources []config.ResourceSpec) func([]int64, []int64) int64 {
	rawScoringFunction := helper.BuildBrokenLinearFunction(scoringFunctionShape)
	resourceScoringFunction := func(requested, capacity int64) int64 {
		if capacity == 0 || requested > capacity {
            // 没容量了直接返回
			return rawScoringFunction(maxUtilization)
		}

		return rawScoringFunction(requested * maxUtilization / capacity)
	}
	return func(requested, allocable []int64) int64 {
		var nodeScore, weightSum int64
		for i := range requested {
			if allocable[i] == 0 {
				continue
			}
			weight := resources[i].Weight
			resourceScore := resourceScoringFunction(requested[i], allocable[i])
			if resourceScore > 0 {
                // 计算权重
				nodeScore += resourceScore * weight
				weightSum += weight
			}
		}
		if weightSum == 0 {
			return 0
		}
        // 最终会算出平均分， 并取整
		return int64(math.Round(float64(nodeScore) / float64(weightSum)))
	}
}
```
实际计算的效果， 可以参考官网的示例：[https://kubernetes.io/zh-cn/docs/concepts/scheduling-eviction/resource-bin-packing/](https://kubernetes.io/zh-cn/docs/concepts/scheduling-eviction/resource-bin-packing/)

## InterPodAffinity
来看一下pod亲和性插件`InterPodAffinity`。 硬亲和性(RequiredAffinity)影响过滤（Filter）结果, 软亲和性(PreferedAffinity)影响打分（Score）结果。
### PreFilter
```go
// PreFilter主要是为亲和性计算所需要的一些数据做准备工作
func (pl *InterPodAffinity) PreFilter(ctx context.Context, cycleState *framework.CycleState, pod *v1.Pod) (*framework.PreFilterResult, *framework.Status) {
	var nodesWithRequiredAntiAffinityPods []*framework.NodeInfo
    // 获取nonde信息
	if nodesWithRequiredAntiAffinityPods, err = pl.sharedLister.NodeInfos().HavePodsWithRequiredAntiAffinityList(); err != nil {
		return nil, framework.AsStatus(fmt.Errorf("failed to list NodeInfos with pods with affinity: %w", err))
	}
	s := &preFilterState{}
    // pod必须亲和性namespace设置
	for i := range s.podInfo.RequiredAffinityTerms {
		if err := pl.mergeAffinityTermNamespacesIfNotEmpty(&s.podInfo.RequiredAffinityTerms[i]); err != nil {
			return nil, framework.AsStatus(err)
		}
	}
    // pod必须反亲和性namespace设置
	for i := range s.podInfo.RequiredAntiAffinityTerms {
		if err := pl.mergeAffinityTermNamespacesIfNotEmpty(&s.podInfo.RequiredAntiAffinityTerms[i]); err != nil {
			return nil, framework.AsStatus(err)
		}
	}  
    // 
    // 软亲和性不会直接影响调度失败，所以这里不做处理
    //
	s.namespaceLabels = GetNamespaceLabelsSnapshot(pod.Namespace, pl.nsLister)
    // 获取一些计算需要数量信息
	s.existingAntiAffinityCounts = pl.getExistingAntiAffinityCounts(ctx, pod, s.namespaceLabels, nodesWithRequiredAntiAffinityPods)
	s.affinityCounts, s.antiAffinityCounts = pl.getIncomingAffinityAntiAffinityCounts(ctx, s.podInfo, allNodes)

	if len(s.existingAntiAffinityCounts) == 0 && len(s.podInfo.RequiredAffinityTerms) == 0 && len(s.podInfo.RequiredAntiAffinityTerms) == 0 {
		return nil, framework.NewStatus(framework.Skip)
	}
    // 将计算结果写入state
	cycleState.Write(preFilterStateKey, s)
	return nil, nil
}
// 实际是下面的 AddPod 和 RemovePod 方法
func (pl *InterPodAffinity) PreFilterExtensions() framework.PreFilterExtensions {
	return pl
}
func (pl *InterPodAffinity) AddPod(ctx context.Context, cycleState *framework.CycleState, podToSchedule *v1.Pod, podInfoToAdd *framework.PodInfo, nodeInfo *framework.NodeInfo) *framework.Status {
	state, err := getPreFilterState(cycleState)
	if err != nil {
		return framework.AsStatus(err)
	}
    // pod亲和性需要设置topologyKey
    // 这里实际是对topologyKey区域的pod数量进行计数（也包含其他的亲和性计算依赖的数据）
    // AddPod在成功时回调， 数量+1
	state.updateWithPod(podInfoToAdd, nodeInfo.Node(), 1)
	return nil
}
func (pl *InterPodAffinity) RemovePod(ctx context.Context, cycleState *framework.CycleState, podToSchedule *v1.Pod, podInfoToRemove *framework.PodInfo, nodeInfo *framework.NodeInfo) *framework.Status {
	state, err := getPreFilterState(cycleState)
	if err != nil {
		return framework.AsStatus(err)
	}
    // RemovePod在成功时回调， 数量-1
	state.updateWithPod(podInfoToRemove, nodeInfo.Node(), -1)
	return nil
}
```
### Filter
`Filter`从三个方面检查pod亲和性是否满足
  - 亲和性检查
    1. 检查指定TopologyKey在node上是否存在，不存在则直接拒绝。
    2. 如果node上没有相关pod则直接调度， 反之对pod进行匹配检查。
  - 反亲和性检查
    1. 检查指定TopologyKey在node上是否存在，不存在则直接拒绝。
    2. 计算匹配的反亲和pod数量， 数量大于0则拒绝。
  - 是否会对其他现有pod产生影响
    1. 检查指定TopologyKey在node上是否存在，不存在则直接拒绝。
    2. 上面的检查是再当前pod的角度去检查， 这里是在这个node的其他pod的角度进行检查
```go
func (pl *InterPodAffinity) Filter(ctx context.Context, cycleState *framework.CycleState, pod *v1.Pod, nodeInfo *framework.NodeInfo) *framework.Status {
	if nodeInfo.Node() == nil {
		return framework.NewStatus(framework.Error, "node not found")
	}
    // 获取PreFilter计算的结果
	state, err := getPreFilterState(cycleState)
	if err != nil {
		return framework.AsStatus(err)
	}
    // 检查pod亲和性
	if !satisfyPodAffinity(state, nodeInfo) {
		return framework.NewStatus(framework.UnschedulableAndUnresolvable, ErrReasonAffinityRulesNotMatch)
	}
    // 检查pod反亲和性
	if !satisfyPodAntiAffinity(state, nodeInfo) {
		return framework.NewStatus(framework.Unschedulable, ErrReasonAntiAffinityRulesNotMatch)
	}
    // 将当前pod调度到此节点， 是否会对node上其他现有pod产生影响
	if !satisfyExistingPodsAntiAffinity(state, nodeInfo) {
		return framework.NewStatus(framework.Unschedulable, ErrReasonExistingAntiAffinityRulesNotMatch)
	}

	return nil
}
```
### PreScore
`PreScore`中将分别计算pod亲和性和反亲和性的分数， 并将结果写入state。
```go
func (pl *InterPodAffinity) PreScore(
	// 省略一些参数
) *framework.Status {
	// 省略一些声明代码

	// 如果没有配置软亲和性， 则跳过
	if pl.args.IgnorePreferredTermsOfExistingPods && !hasPreferredAffinityConstraints && !hasPreferredAntiAffinityConstraints {
		cycleState.Write(preScoreStateKey, &preScoreState{
			topologyScore: make(map[string]map[string]int64),
		})
		return nil
	}
    // 省略一些声明代码
    // 
	// 存储计算结果
	state := &preScoreState{
        // 计算结果以TopologyKey为分区存储
        // m[term.TopologyKey][tpValue] += int64(weight * multiplier)
        // {"TopologyKey": {"tpValue": int64(weight * multiplier), "zone": {"zone1": 1, "zone2": 2}}}
		topologyScore: make(map[string]map[string]int64),
	}

    // 省略namespace合并代码

	topoScores := make([]scoreMap, len(allNodes))
	index := int32(-1)
	processNode := func(i int) {
		// 省略一些声明代码
        // 
		topoScore := make(scoreMap)
		for _, existingPod := range podsToProcess {
            // 计算函数
			pl.processExistingPod(state, existingPod, nodeInfo, pod, topoScore)
		}
		if len(topoScore) > 0 {
            // 加入到结果集
			topoScores[atomic.AddInt32(&index, 1)] = topoScore
		}
	}
    // 并发计算
	pl.parallelizer.Until(pCtx, len(allNodes), processNode, pl.Name())

	for i := 0; i <= int(index); i++ {
		state.topologyScore.append(topoScores[i])
	}
    // 将计算结果写入state
	cycleState.Write(preScoreStateKey, state)
	return nil
}
func (pl *InterPodAffinity) processExistingPod(
	state *preScoreState,
	existingPod *framework.PodInfo,
	existingPodNodeInfo *framework.NodeInfo,
	incomingPod *v1.Pod,
	topoScore scoreMap,
) {
	existingPodNode := existingPodNodeInfo.Node()
	if len(existingPodNode.Labels) == 0 {
		return
	}
    // 以当前的pod为基准， 计算亲和性和反亲和性的分数 
    // 符合亲和性会加分
	topoScore.processTerms(state.podInfo.PreferredAffinityTerms, existingPod.Pod, nil, existingPodNode, 1)
    // 符合反亲和性会减分
	topoScore.processTerms(state.podInfo.PreferredAntiAffinityTerms, existingPod.Pod, nil, existingPodNode, -1)
	if pl.args.HardPodAffinityWeight > 0 && len(existingPodNode.Labels) != 0 {
		for _, t := range existingPod.RequiredAffinityTerms {
			topoScore.processTerm(&t, pl.args.HardPodAffinityWeight, incomingPod, state.namespaceLabels, existingPodNode, 1)
		}
	}
    // 以node上已经存在的pod为基准， 计算亲和性和反亲和性的分数
	topoScore.processTerms(existingPod.PreferredAffinityTerms, incomingPod, state.namespaceLabels, existingPodNode, 1)
	topoScore.processTerms(existingPod.PreferredAntiAffinityTerms, incomingPod, state.namespaceLabels, existingPodNode, -1)
}
// 计算
func (m scoreMap) processTerm(term *framework.AffinityTerm, weight int32, pod *v1.Pod, nsLabels labels.Set, node *v1.Node, multiplier int32) {
	if term.Matches(pod, nsLabels) {
		if tpValue, tpValueExist := node.Labels[term.TopologyKey]; tpValueExist {
			if m[term.TopologyKey] == nil {
				m[term.TopologyKey] = make(map[string]int64)
			}
            // 权重 * 倍数 1｜-1
			m[term.TopologyKey][tpValue] += int64(weight * multiplier)
		}
	}
}
```
### Score
`Score`没有什么好说的， 直接使用`PreScore`计算的结果。
```go
func (pl *InterPodAffinity) Score(ctx context.Context, cycleState *framework.CycleState, pod *v1.Pod, nodeName string) (int64, *framework.Status) {
    // 省略获取node信息， state
	var score int64
	for tpKey, tpValues := range s.topologyScore {
		if v, exist := node.Labels[tpKey]; exist {
            // score有可能会很大， 超过最大分的限制
			score += tpValues[v]
		}
	}
	return score, nil
}
```
### NormalizeScore
`NormalizeScore`会将分数归一化， 使其在0~MaxNodeScore之间。
```go
func (pl *InterPodAffinity) NormalizeScore(ctx context.Context, cycleState *framework.CycleState, pod *v1.Pod, scores framework.NodeScoreList) *framework.Status {
	// 省略一些声明代码
	var minCount int64 = math.MaxInt64
	var maxCount int64 = math.MinInt64
	for i := range scores {
		score := scores[i].Score
		if score > maxCount {
			maxCount = score
		}
		if score < minCount {
			minCount = score
		}
	}
    // 计算最大分和最小分的差值
	maxMinDiff := maxCount - minCount
	for i := range scores {
		fScore := float64(0)
		if maxMinDiff > 0 {
            // 将分数归一化
			fScore = float64(framework.MaxNodeScore) * (float64(scores[i].Score-minCount) / float64(maxMinDiff))
		}
		scores[i].Score = int64(fScore)
	}
	return nil
}
```
## VolumeBinding
`VolumeBinding`实现了`PreFilter`、`Filter`、`Score`、`Reserve`、`PreBind`五个接口， 是一个非常重要的插件。

这个插件在pod需要pvc资源时才会启用，他根据pvc、pv、sc等信息决策调度结果。在涉及到StorageClass（sc）时，会有一些特殊的处理。sc创建时会声明两种绑定模式：
1. Immediate：立即绑定，创建pvc时会立即绑定pv。
2. WaitForFirstConsumer：延迟绑定，创建pvc时不会立即绑定pv，而是等到pod创建时才会绑定pv。
### PreFilter
对计算所用到的数据进行前期处理和写入。
```go
func (pl *VolumeBinding) PreFilter(ctx context.Context, state *framework.CycleState, pod *v1.Pod) (*framework.PreFilterResult, *framework.Status) {
	// 如果pod没有pvc，直接跳过
	if hasPVC, err := pl.podHasPVCs(pod); err != nil {
		return nil, framework.NewStatus(framework.UnschedulableAndUnresolvable, err.Error())
	} else if !hasPVC {
		state.Write(stateKey, &stateData{})
        // framework.Skip是一个特殊错误，表示跳过
		return nil, framework.NewStatus(framework.Skip)
	}
    // GetPodVolumeClaims会获取pod的pvc信息，包括已经绑定的pvc和需要延迟绑定的pvc
	podVolumeClaims, err := pl.Binder.GetPodVolumeClaims(pod)
	if err != nil {
		return nil, framework.AsStatus(err)
	}
	if len(podVolumeClaims.unboundClaimsImmediate) > 0 {
		// 需要立即绑定的pvc没有绑定pv，直接返回错误
        // 绑定策略有两种模式， 立即绑定和延迟绑定。简单来说就是pvc与pv绑定的过程放在调度之前还是之后
		status := framework.NewStatus(framework.UnschedulableAndUnresolvable)
		status.AppendReason("pod has unbound immediate PersistentVolumeClaims")
		return nil, status
	}
	var result *framework.PreFilterResult
    // 获取符合条件的node
	if eligibleNodes := pl.Binder.GetEligibleNodes(podVolumeClaims.boundClaims); eligibleNodes != nil {
		result = &framework.PreFilterResult{
			NodeNames: eligibleNodes,
		}
	}
    // 将结果写入state
	state.Write(stateKey, &stateData{
		podVolumesByNode: make(map[string]*PodVolumes),
		podVolumeClaims: &PodVolumeClaims{
            // 已经绑定的pvc
			boundClaims:                podVolumeClaims.boundClaims,
            // 需要延迟绑定的pvc
			unboundClaimsDelayBinding:  podVolumeClaims.unboundClaimsDelayBinding,
            // 延迟绑定的pv
			unboundVolumesDelayBinding: podVolumeClaims.unboundVolumesDelayBinding,
		},
	})
	return result, nil
}
func (pl *VolumeBinding) PreFilterExtensions() framework.PreFilterExtensions {
    // VolumeBinding不需要回调， 所以返回nil
	return nil
}
// pl.Binder.GetEligibleNodes(podVolumeClaims.boundClaims)
func (b *volumeBinder) GetPodVolumeClaims(pod *v1.Pod) (podVolumeClaims *PodVolumeClaims, err error) {
	podVolumeClaims = &PodVolumeClaims{
        // 已经绑定的pvc
		boundClaims:               []*v1.PersistentVolumeClaim{},
        // 需要立即绑定的pvc
		unboundClaimsImmediate:    []*v1.PersistentVolumeClaim{},
        // 需要延迟绑定的pvc
		unboundClaimsDelayBinding: []*v1.PersistentVolumeClaim{},
	}

	for _, vol := range pod.Spec.Volumes {
		volumeBound, pvc, err := b.isVolumeBound(pod, &vol)
		if volumeBound {
			podVolumeClaims.boundClaims = append(podVolumeClaims.boundClaims, pvc)
		} else {
            // 这里会根据pvc中stroageClassName获取sc的绑定模式，将pvc分为立即绑定和延迟绑定两种
			delayBindingMode, err := volume.IsDelayBindingMode(pvc, b.classLister)
			if err != nil {
				return podVolumeClaims, err
			}
			if delayBindingMode && pvc.Spec.VolumeName == "" {
				podVolumeClaims.unboundClaimsDelayBinding = append(podVolumeClaims.unboundClaimsDelayBinding, pvc)
			} else {
				podVolumeClaims.unboundClaimsImmediate = append(podVolumeClaims.unboundClaimsImmediate, pvc)
			}
		}
	}
    // 如果有延迟绑定的pvc， 获取系统中已经存在的可能可以与其绑定的pv。
    // 这个信息在后面Filter阶段会用到
	podVolumeClaims.unboundVolumesDelayBinding = map[string][]*v1.PersistentVolume{}
	for _, pvc := range podVolumeClaims.unboundClaimsDelayBinding {
		// {"scName1": [unboundPV1, unboundPV2], "scName2": [unboundPV3]}
		storageClassName := volume.GetPersistentVolumeClaimClass(pvc)
		podVolumeClaims.unboundVolumesDelayBinding[storageClassName] = b.pvCache.ListPVs(storageClassName)
	}
	return podVolumeClaims, nil
}
```
### Filter
`Filter`从以下几个方面进行判断：
- 已经处于绑定状态的pvc
  - 其pv的node亲和性是否满足需求， 不满足则拒绝
- 未绑定的pvc（延迟绑定）
  - annotation中`volume.kubernetes.io/selected-node`是否与node匹配， 不匹配则拒绝
  - pvc指定的sc下，已经存在的pv是否有满足需求的，有则和pvc绑定.pv会根据容量升序排序，所以会优先绑定较小的pv。剩下未绑定的继续下一步判断 
  - 剩下的pvc意味着需要使用sc动态申请pv， 将依次检查：
    - sc是否支持动态申请pv， 不支持则拒绝 。通过添加`kubernetes.io/no-provisioner`实现。
    - sc是否支持当前node， 不支持则拒绝。
    - sc是否有足够的空间， 不足则拒绝。
这里具体运行的函数逻辑都不复杂，就不展开了， 主要是看一下处理流程，实现如下：
```go
func (pl *VolumeBinding) Filter(ctx context.Context, cs *framework.CycleState, pod *v1.Pod, nodeInfo *framework.NodeInfo) *framework.Status {
    // 获取PreFilter计算的结果
	state, err := getStateData(cs)
	if err != nil {
		return framework.AsStatus(err)
	}
    // 获取pod的pvc信息, 分为静态绑定和动态绑定两种
	podVolumes, reasons, err := pl.Binder.FindPodVolumes(pod, state.podVolumeClaims, node)
    // 如果有错误， 则直接返回
    if err != nil {
		return framework.AsStatus(err)
	}
	state.Lock()
    // 将pvc信息写入state
	state.podVolumesByNode[node.Name] = podVolumes
	state.Unlock()
	return nil
}
// pl.Binder.FindPodVolumes
func (b *volumeBinder) FindPodVolumes(pod *v1.Pod, podVolumeClaims *PodVolumeClaims, node *v1.Node) (podVolumes *PodVolumes, reasons ConflictReasons, err error) {
    // 未绑定的卷是否满足
	unboundVolumesSatisfied := true
    // 已经绑定的卷是否满足
	boundVolumesSatisfied := true
    // 存储空间是否足够
	sufficientStorage := true
    // pv资源是否可以与pvc匹配
	boundPVsFound := true
	defer func() {
        // 根据不同的状态添加错误信息
		if !boundVolumesSatisfied {
			reasons = append(reasons, ErrReasonNodeConflict)
		}
		// 省略其他状态的错误信息添加
	}()
    // 省略一些代码

    // 已经与pv绑定的pvc， 其pv所在node是否和当前node匹配
	if len(podVolumeClaims.boundClaims) > 0 {
		boundVolumesSatisfied, boundPVsFound, err = b.checkBoundClaims(podVolumeClaims.boundClaims, node, pod)
		if err != nil {
			return
		}
	}
	// 延迟绑定的pvc处理
	if len(podVolumeClaims.unboundClaimsDelayBinding) > 0 {
		var (
			claimsToFindMatching []*v1.PersistentVolumeClaim
			claimsToProvision    []*v1.PersistentVolumeClaim
		)
		for _, claim := range podVolumeClaims.unboundClaimsDelayBinding {
			if selectedNode, ok := claim.Annotations[volume.AnnSelectedNode]; ok {
				if selectedNode != node.Name {
					// pvc存在"volume.AnnSelectedNode"标签，代表已经被当前或其他scheduler调度过
					// 如果其value与当前nodename不匹配，则拒绝
					unboundVolumesSatisfied = false
					return
				}
				claimsToProvision = append(claimsToProvision, claim)
			} else {
				claimsToFindMatching = append(claimsToFindMatching, claim)
			}
		}
		if len(claimsToFindMatching) > 0 {
			var unboundClaims []*v1.PersistentVolumeClaim
            // 尝试与已经存在的pv进行绑定， （静态绑定）
            // 这可能会剩下一下绑定不了的pvc， 没关系， 会交给下一步的动态绑定
			unboundVolumesSatisfied, staticBindings, unboundClaims, err = b.findMatchingVolumes(pod, claimsToFindMatching, podVolumeClaims.unboundVolumesDelayBinding, node)
			if err != nil {
				return
			}
			claimsToProvision = append(claimsToProvision, unboundClaims...)
		}
		if len(claimsToProvision) > 0 {
            // 尝试进行动态绑定（动态向sc申请pv资源）
			unboundVolumesSatisfied, sufficientStorage, dynamicProvisions, err = b.checkVolumeProvisions(pod, claimsToProvision, node)
			if err != nil {
				return
			}
		}
	}
	return
}
```
### Score
`Score`主要是对Filter阶段的结果进行打分
```go
func (pl *VolumeBinding) Score(ctx context.Context, cs *framework.CycleState, pod *v1.Pod, nodeName string) (int64, *framework.Status) {
	// 省略数据准备代码
	// 对storageClass进行分组 ， key为storageClass的名称， value为storageClass的资源信息
	classResources := make(classResourceMap)
	for _, staticBinding := range podVolumes.StaticBindings {
		class := staticBinding.StorageClassName()
		storageResource := staticBinding.StorageResource()
		if _, ok := classResources[class]; !ok {
			classResources[class] = &StorageResource{
				Requested: 0,
				Capacity:  0,
			}
		}
        // 已经请求的容量
		classResources[class].Requested += storageResource.Requested
        // 总容量
		classResources[class].Capacity += storageResource.Capacity
	}
	return pl.scorer(classResources), nil
}
```
`pl.scorer`是具体的打分函数。具体实现是`buildScorerFunction`
```go
// pkg/scheduler/framework/plugins/volumebinding/scorer.go
func buildScorerFunction(scoringFunctionShape helper.FunctionShape) volumeCapacityScorer {
	rawScoringFunction := helper.BuildBrokenLinearFunction(scoringFunctionShape)
	f := func(requested, capacity int64) int64 {
		if capacity == 0 || requested > capacity {
			return rawScoringFunction(maxUtilization)
		}
        // requested * maxUtilization / capacity 计算利用率百分比， 当前使用了多少
		// rawScoringFunction 将利用率百分比转换为分数
		return rawScoringFunction(requested * maxUtilization / capacity)
	}
	return func(classResources classResourceMap) int64 {
		var nodeScore int64
		// sc数量即权重
		weightSum := len(classResources)
		if weightSum == 0 {
			return 0
		}
		for _, resource := range classResources {
			classScore := f(resource.Requested, resource.Capacity)
			nodeScore += classScore
		}
        // 实际是计算sc的平均分数
		return int64(math.Round(float64(nodeScore) / float64(weightSum)))
	}
}
```
### Reserve
`Reserve`会检查pvc的状态， 并更新`state`和`cache`的数据。 
```go
func (pl *VolumeBinding) Reserve(ctx context.Context, cs *framework.CycleState, pod *v1.Pod, nodeName string) *framework.Status {
    // 省略不重要的代码
		allBound, err := pl.Binder.AssumePodVolumes(pod, nodeName, podVolumes)
		if err != nil {
			return framework.AsStatus(err)
		}
        // pvc是否已经都处于绑定状态
		state.allBound = allBound
    // 省略不重要的代码
}
// pl.Binder.AssumePodVolumes
func (b *volumeBinder) AssumePodVolumes(assumedPod *v1.Pod, nodeName string, podVolumes *PodVolumes) (allFullyBound bool, err error) {
    // 如果全部已经绑定， 直接返回
	if allBound := b.arePodVolumesBound(assumedPod); allBound {
		return true, nil
	}
	newBindings := []*BindingInfo{}
    // 检查静态绑定卷的状态
    // 实际绑定的动作是由controller监听到binding资源，然后去执行的绑定操作
	for _, binding := range podVolumes.StaticBindings {
        // 这里会检测绑定状态， 未绑定会将pvc设置为需要controller绑定
        // pvc绑定有两种，
        // 一种是由用户指定的
        // 一种是由controller自动绑定的
		newPV, dirty, err := volume.GetBindVolumeToClaim(binding.pv, binding.pvc)
		if err != nil {
			klog.ErrorS(err, "AssumePodVolumes: fail to GetBindVolumeToClaim")
			b.revertAssumedPVs(newBindings)
			return false, err
		}
		if dirty {
            // pvCache是本地的缓存。
            // 因为数据变动操作是在当前发生的，所以理论上infomer的数据会在某些时候慢于本地cache
            // 这里会先用本地cache记录最新的，可能的数据状态， 然后等待infomer的数据更新
			err = b.pvCache.Assume(newPV)
			if err != nil {
                // 如果失败， 则回滚
				b.revertAssumedPVs(newBindings)
				return false, err
			}
		}
		newBindings = append(newBindings, &BindingInfo{pv: newPV, pvc: binding.pvc})
	}

	newProvisionedPVCs := []*v1.PersistentVolumeClaim{}
    // 检查动态绑定卷的状态
	for _, claim := range podVolumes.DynamicProvisions {
        // claim在缓存中共享， 所以这里copy一份
		claimClone := claim.DeepCopy()
        // 更改pvc的Annotation， 增加 “volume.kubernetes.io/selected-node”,
		// 代表此pvc绑定过程已由schecduler触发，并且已经分配对应节点 
		metav1.SetMetaDataAnnotation(&claimClone.ObjectMeta, volume.AnnSelectedNode, nodeName)
		err = b.pvcCache.Assume(claimClone)
		if err != nil {
			b.revertAssumedPVs(newBindings)
			b.revertAssumedPVCs(newProvisionedPVCs)
			return
		}

		newProvisionedPVCs = append(newProvisionedPVCs, claimClone)
	}

	podVolumes.StaticBindings = newBindings
	podVolumes.DynamicProvisions = newProvisionedPVCs
	return
}
// 在reserve及之后的阶段失败时， 将会调用这个方法进行回滚
func (pl *VolumeBinding) Unreserve(ctx context.Context, cs *framework.CycleState, pod *v1.Pod, nodeName string) {
	// 省略不重要的代码
    // 会滚数据. 主要是对本地cache数据做处理， 将其恢复到infomer中的数据版本
	pl.Binder.RevertAssumedPodVolumes(podVolumes)
}
```
### PreBind
`PreBind`可以说是`VolumeBinding`插件的“独有”方法。(因为别的插件都不需要...)
```go
func (pl *VolumeBinding) PreBind(ctx context.Context, cs *framework.CycleState, pod *v1.Pod, nodeName string) *framework.Status {
	// 省略不重要的代码
    // 调用pv和pvc的更新接口， 将绑定信息写入到apiserver->etcd中
    // 如果更新失败， 则直接返回错误
    // b.kubeClient.CoreV1().PersistentVolumes().Update(ctx, binding.pv, metav1.UpdateOptions{})
    // b.kubeClient.CoreV1().PersistentVolumeClaims(claim.Namespace).Update(ctx, claim, metav1.UpdateOptions{})
	err = pl.Binder.BindPodVolumes(ctx, pod, podVolumes)
	if err != nil {
		return framework.AsStatus(err)
	}
	return nil
}
```
## 其他
`scheduler`中还内置了其他的一些插件，比如:
- `ImageLocality` 根据node上的镜像缓存情况，为node进行打分
- `NodeAffinity` 根据pod中指定的node亲和性，过滤、打分
- `NodePort` 将pod.container.port的数量与node上的端口情况进行比对，过滤node
- 等等...

默认情况下， 如`ImageLocality`这种“不太重要”的插件，占用的权重比会较低， 但是可以通过配置文件进行调整。但是不要轻视这些插件，有可能会是“压死骆驼的最后一根稻草”～～～～