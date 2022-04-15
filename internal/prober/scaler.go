package prober

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/gardener/dependency-watchdog/internal/util"
	"github.com/gardener/gardener/pkg/utils/flow"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	scalev1 "k8s.io/client-go/scale"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	ignoreScalingAnnotationKey        = "dependency-watchdog.gardener.cloud/ignore-scaling"
	defaultMaxResourceScalingAttempts = 3
	defaultScaleResourceBackoff       = 100 * time.Millisecond
)

type DeploymentScaler interface {
	ScaleUp(ctx context.Context) error
	ScaleDown(ctx context.Context) error
}

func NewDeploymentScaler(namespace string, config *Config, client client.Client, scalerGetter scalev1.ScalesGetter) DeploymentScaler {
	ds := deploymentScaler{
		namespace: namespace,
		scaler:    scalerGetter.Scales(namespace),
		client:    client,
	}
	ds.scaleDownFlow = ds.createResourceScaleFlow(namespace, fmt.Sprintf("scale-down-%s", namespace), createScaleDownResourceInfos(config.DependentResourceInfos), util.ScaleDownReplicasMismatch)
	ds.scaleUpFlow = ds.createResourceScaleFlow(namespace, fmt.Sprintf("scale-up-%s", namespace), createScaleUpResourceInfos(config.DependentResourceInfos), util.ScaleUpReplicasMismatch)
	return &ds
}

// scaleableResourceInfo contains a flattened scaleUp or scaleDown resource info for a given resource reference
type scaleableResourceInfo struct {
	ref          autoscalingv1.CrossVersionObjectReference
	level        int
	initialDelay time.Duration
	timeout      time.Duration
	replicas     int32
}

type mismatchReplicasCheckFn func(replicas, targetReplicas int32) bool

type deploymentScaler struct {
	namespace     string
	scaler        scalev1.ScaleInterface
	client        client.Client
	scaleDownFlow *flow.Flow
	scaleUpFlow   *flow.Flow
}

func (ds *deploymentScaler) ScaleDown(ctx context.Context) error {
	return ds.scaleDownFlow.Run(ctx, flow.Opts{})
}

func (ds *deploymentScaler) ScaleUp(ctx context.Context) error {
	return ds.scaleUpFlow.Run(ctx, flow.Opts{})
}

func isIgnoreScalingAnnotationSet(deployment *appsv1.Deployment) bool {
	if val, ok := deployment.Annotations[ignoreScalingAnnotationKey]; ok {
		return val == "true"
	}
	return false
}

func (ds *deploymentScaler) createResourceScaleFlow(namespace, flowName string, resourceInfos []scaleableResourceInfo, mismatchReplicasCheckFn func(replicas, targetReplicas int32) bool) *flow.Flow {
	levels := sortAndGetUniqueLevels(resourceInfos)
	orderedResourceInfos := collectResourceInfosByLevel(resourceInfos)
	g := flow.NewGraph(flowName)
	var previousLevelResourceInfos []scaleableResourceInfo
	for _, level := range levels {
		var previousTaskID flow.TaskID
		if resInfos, ok := orderedResourceInfos[level]; ok {
			taskID := g.Add(flow.Task{
				Name:         fmt.Sprintf("scaling dependencies %v at level %d", resInfos, level),
				Fn:           ds.createScaleTaskFn(namespace, resInfos, mismatchReplicasCheckFn, previousLevelResourceInfos),
				Dependencies: flow.NewTaskIDs(previousTaskID),
			})
			copy(previousLevelResourceInfos, resInfos)
			previousTaskID = taskID
		}
	}
	return g.Compile()
}

// createScaleTaskFn creates a flow.TaskFn for a slice of DependentResourceInfo. If there are more than one
// DependentResourceInfo passed to this function, it indicates that they all are at the same level indicating that these functions
// should be invoked concurrently. In this case it will construct a flow.Parallel. If there is only one DependentResourceInfo passed
// then it indicates that at a specific level there is only one DependentResourceInfo that needs to be scaled.
func (ds *deploymentScaler) createScaleTaskFn(namespace string, resourceInfos []scaleableResourceInfo, mismatchReplicasCheckFn func(replicas, targetReplicas int32) bool, waitOnResourceInfos []scaleableResourceInfo) flow.TaskFn {
	if len(resourceInfos) == 0 {
		logger.V(4).Info("(createScaleTaskFn) [unexpected] resourceInfos. This should never be the case.", "namespace", namespace)
		return nil
	}
	taskFns := make([]flow.TaskFn, len(resourceInfos))
	for _, resourceInfo := range resourceInfos {
		taskFn := flow.TaskFn(func(ctx context.Context) error {
			operation := fmt.Sprintf("scale-resource-%s.%s", namespace, resourceInfo.ref.Name)
			result := util.Retry(ctx,
				operation,
				func() (interface{}, error) {
					err := ds.scale(ctx, resourceInfo, mismatchReplicasCheckFn, waitOnResourceInfos)
					return nil, err
				},
				defaultMaxResourceScalingAttempts,
				defaultGetSecretBackoff,
				util.AlwaysRetry)
			logger.V(4).Info("resource has been scaled", "namespace", namespace, "resource", resourceInfo)
			return result.Err
		})
		taskFns = append(taskFns, taskFn)
	}
	if len(resourceInfos) == 1 {
		return taskFns[0]
	}
	return flow.Parallel(taskFns...)
}

func (ds *deploymentScaler) scale(ctx context.Context, resourceInfo scaleableResourceInfo, mismatchReplicas mismatchReplicasCheckFn, waitOnResourceInfos []scaleableResourceInfo) error {
	deployment, err := util.GetDeploymentFor(ctx, ds.namespace, resourceInfo.ref.Name, ds.client)
	if err != nil {
		logger.Error(err, "error getting deployment for resource, skipping scaling operation", "namespace", ds.namespace, "resourceInfo", resourceInfo)
		return err
	}
	// sleep for initial delay
	err = util.SleepWithContext(ctx, resourceInfo.initialDelay)
	if err != nil {
		logger.Error(err, "looks like the context has been cancelled. exiting scaling operation", "namespace", ds.namespace, "resourceInfo", resourceInfo)
		return err
	}
	if ds.shouldScale(ctx, deployment, resourceInfo.replicas, mismatchReplicas, waitOnResourceInfos) {
		util.Retry(ctx, fmt.Sprintf(""), func() (*autoscalingv1.Scale, error) {
			return ds.doScale(ctx, resourceInfo)
		}, defaultMaxResourceScalingAttempts, defaultScaleResourceBackoff, util.AlwaysRetry)
	}
	return nil
}

func (ds *deploymentScaler) shouldScale(ctx context.Context, deployment *appsv1.Deployment, targetReplicas int32, mismatchReplicas mismatchReplicasCheckFn, waitOnResourceInfos []scaleableResourceInfo) bool {
	if isIgnoreScalingAnnotationSet(deployment) {
		logger.V(4).Info("scaling ignored due to explicit instruction via annotation", "namespace", ds.namespace, "deploymentName", deployment.Name, "annotation", ignoreScalingAnnotationKey)
		return false
	}
	// check the current replicas and compare it against the desired replicas
	deploymentSpecReplicas := *deployment.Spec.Replicas
	if !mismatchReplicas(deploymentSpecReplicas, targetReplicas) {
		logger.V(4).Info("spec replicas matches the target replicas. scaling for this resource is skipped", "namespace", ds.namespace, "deploymentName", deployment.Name, "deploymentSpecReplicas", deploymentSpecReplicas, "targetReplicas", targetReplicas)
		return false
	}
	// check if all resources this resource should wait on have been scaled, if not then we cannot scale this resource.
	// Check for currently available replicas and not the desired replicas on the upstream resource dependencies.
	if waitOnResourceInfos != nil {
		for _, upstreamDependentResource := range waitOnResourceInfos {
			upstreamDeployment, err := util.GetDeploymentFor(ctx, ds.namespace, upstreamDependentResource.ref.Name, ds.client)
			if err != nil {
				logger.Error(err, "failed to get deployment for upstream dependent resource, skipping scaling", "upstreamDependentResource", upstreamDependentResource)
				return false
			}
			actualReplicas := upstreamDeployment.Status.Replicas
			if mismatchReplicas(actualReplicas, upstreamDependentResource.replicas) {
				logger.V(4).Info("upstream resource has still not been scaled to the desired replicas, skipping scaling of resource", "namespace", ds.namespace, "deploymentToScale", deployment.Name, "upstreamResourceInfo", upstreamDependentResource, "actualReplicas", actualReplicas)
				return false
			}
		}
	}
	return true
}

func (ds *deploymentScaler) doScale(ctx context.Context, resourceInfo scaleableResourceInfo) (*autoscalingv1.Scale, error) {
	gr, err := ds.getGroupResource(resourceInfo.ref)
	if err != nil {
		return nil, err
	}
	scale, err := ds.scaler.Get(ctx, gr, resourceInfo.ref.Name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	scale.Spec.Replicas = resourceInfo.replicas
	return ds.scaler.Update(ctx, gr, scale, metav1.UpdateOptions{})
}

func (ds *deploymentScaler) getGroupResource(resourceRef autoscalingv1.CrossVersionObjectReference) (schema.GroupResource, error) {
	gv, _ := schema.ParseGroupVersion(resourceRef.APIVersion) // Ignoring the error as this validation has already been done when initially validating the Config
	gk := schema.GroupKind{
		Group: gv.Group,
		Kind:  resourceRef.Kind,
	}
	mapping, err := ds.client.RESTMapper().RESTMapping(gk, gv.Version)
	if err != nil {
		logger.Error(err, "failed to get RESTMapping for resource", "resourceRef", resourceRef)
		return schema.GroupResource{}, err
	}
	return mapping.Resource.GroupResource(), nil
}

func collectResourceInfosByLevel(resourceInfos []scaleableResourceInfo) map[int][]scaleableResourceInfo {
	resInfosByLevel := make(map[int][]scaleableResourceInfo)
	for _, resInfo := range resourceInfos {
		level := resInfo.level
		if _, ok := resInfosByLevel[level]; !ok {
			var levelResInfos []scaleableResourceInfo
			levelResInfos = append(levelResInfos, resInfo)
			resInfosByLevel[level] = levelResInfos
		} else {
			resInfosByLevel[level] = append(resInfosByLevel[level], resInfo)
		}
	}
	return resInfosByLevel
}

func sortAndGetUniqueLevels(resourceInfos []scaleableResourceInfo) []int {
	var levels []int
	keys := make(map[int]bool)
	for _, resInfo := range resourceInfos {
		if _, found := keys[resInfo.level]; !found {
			keys[resInfo.level] = true
			levels = append(levels, resInfo.level)
		}
	}
	sort.Ints(levels)
	return levels
}

func createScaleUpResourceInfos(dependentResourceInfos []DependentResourceInfo) []scaleableResourceInfo {
	resourceInfos := make([]scaleableResourceInfo, 0, len(dependentResourceInfos))
	for _, depResInfo := range dependentResourceInfos {
		resInfo := scaleableResourceInfo{
			ref:          depResInfo.Ref,
			level:        depResInfo.ScaleUpInfo.Level,
			initialDelay: *depResInfo.ScaleUpInfo.InitialDelay,
			timeout:      *depResInfo.ScaleUpInfo.Timeout,
			replicas:     *depResInfo.ScaleUpInfo.Replicas,
		}
		resourceInfos = append(resourceInfos, resInfo)
	}
	return resourceInfos
}

func createScaleDownResourceInfos(dependentResourceInfos []DependentResourceInfo) []scaleableResourceInfo {
	resourceInfos := make([]scaleableResourceInfo, 0, len(dependentResourceInfos))
	for _, depResInfo := range dependentResourceInfos {
		resInfo := scaleableResourceInfo{
			ref:          depResInfo.Ref,
			level:        depResInfo.ScaleDownInfo.Level,
			initialDelay: *depResInfo.ScaleDownInfo.InitialDelay,
			timeout:      *depResInfo.ScaleDownInfo.Timeout,
			replicas:     *depResInfo.ScaleDownInfo.Replicas,
		}
		resourceInfos = append(resourceInfos, resInfo)
	}
	return resourceInfos
}