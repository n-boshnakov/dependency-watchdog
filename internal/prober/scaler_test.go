package prober

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/gardener/dependency-watchdog/internal/util"
	"github.com/gardener/gardener/pkg/utils/flow"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/scale"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

var (
	deploymentPath      = filepath.Join("testdata", "deployment.yaml")
	defaultInitialDelay = 10 * time.Millisecond
	defaultTimeout      = 20 * time.Millisecond
	mcmRef              = autoscalingv1.CrossVersionObjectReference{Kind: "Deployment", Name: "machine-controller-manager", APIVersion: "apps/v1"}
	kcmRef              = autoscalingv1.CrossVersionObjectReference{Kind: "Deployment", Name: "kube-controller-manager", APIVersion: "apps/v1"}
	caRef               = autoscalingv1.CrossVersionObjectReference{Kind: "Deployment", Name: "cluster-autoscaler", APIVersion: "apps/v1"}
	kcmDeploy           *appsv1.Deployment
	mcmDeploy           *appsv1.Deployment
	caDeploy            *appsv1.Deployment
	k8sClient           client.Client
	testEnv             *envtest.Environment
	cfg                 *rest.Config
	probeCfg            *Config
	scalesGetter        scale.ScalesGetter
	ds                  DeploymentScaler
	ctx                 = context.Background()
)

const namespace = "default"

func TestScalerSuite(t *testing.T) {
	tests := []struct {
		title string
		run   func(t *testing.T)
	}{
		{"test resource scale flow", testCreateResourceScaleFlow},
		{"test deployment not found", testDeploymentNotFound},
	}
	k8sClient, cfg, testEnv = BeforeSuite(t)
	scalesGetter, _ = util.CreateScalesGetter(cfg)
	createProbeConfig()
	ds = NewDeploymentScaler(namespace, probeCfg, k8sClient, scalesGetter)
	for _, test := range tests {
		t.Run(test.title, func(t *testing.T) {
			test.run(t)
		})
	}
	AfterSuite(t, testEnv)

}
func TestFlow(t *testing.T) {
	func1 := func(ctx context.Context) error {
		log.Println("executing func1")
		time.Sleep(10 * time.Second)
		return nil
	}
	func2 := func(ctx context.Context) error {
		log.Println("executing func2")
		time.Sleep(50 * time.Millisecond)
		return nil
	}
	func3 := func(ctx context.Context) error {
		log.Println("executing func3")
		return nil
	}

	g := flow.NewGraph("test")
	taskId := g.Add(flow.Task{
		Name:         "func1-2",
		Fn:           flow.Parallel(func1, func2),
		Dependencies: nil,
	})
	g.Add(flow.Task{
		Name:         "func3",
		Fn:           func3,
		Dependencies: flow.NewTaskIDs(taskId),
	})

	f := g.Compile()
	f.Run(context.Background(), flow.Opts{})
}
func testCreateResourceScaleFlow(t *testing.T) {
	g := NewWithT(t)

	depScaler := deploymentScaler{
		scaler: scalesGetter.Scales(namespace),
	}
	var scri []scaleableResourceInfo
	scri = append(scri, scaleableResourceInfo{ref: caRef, level: 1, initialDelay: defaultInitialDelay, timeout: defaultTimeout, replicas: 0})
	scri = append(scri, scaleableResourceInfo{ref: mcmRef, level: 0, initialDelay: defaultInitialDelay, timeout: defaultTimeout, replicas: 0})
	scri = append(scri, scaleableResourceInfo{ref: kcmRef, level: 0, initialDelay: defaultInitialDelay, timeout: defaultTimeout, replicas: 0})

	waitOnResourceInfosForCA := []scaleableResourceInfo{
		scri[1],
		scri[2],
	}
	sf := depScaler.createResourceScaleFlow(namespace, "test", scri, util.ScaleDownReplicasMismatch)
	g.Expect(sf).ToNot(BeNil())
	g.Expect(sf.flow).ToNot(BeNil())
	g.Expect(sf.flow.Name()).To(Equal("test"))
	g.Expect(sf.flow.Len()).To(Equal(2))
	g.Expect(len(sf.flowStepInfos)).To(Equal(2))
	g.Expect(sf.flowStepInfos[0].dependentTaskIDs).To(BeNil())
	g.Expect(sf.flowStepInfos[0].waitOnResourceInfos).To(BeNil())
	g.Expect(sf.flowStepInfos[1].dependentTaskIDs.Len()).To(Equal(1))
	_, ok := sf.flowStepInfos[1].dependentTaskIDs[sf.flowStepInfos[0].taskID]
	g.Expect(ok).To(BeTrue())
	g.Expect(sf.flowStepInfos[1].waitOnResourceInfos).To(Equal(waitOnResourceInfosForCA))

}

func testDeploymentNotFound(t *testing.T) {
	//g := NewWithT(t)
	table := []struct {
		mcmReplicas         int32
		caReplicas          int32
		kcmReplicas         int32
		expectedmcmReplicas int32
		expectedcaReplicas  int32
		expectedkcmReplicas int32
		f                   func(context.Context) error
	}{
		//{0, 0, 0, 1, 1, 1, ds.ScaleUp},
		//{0, 1, 0, 1, ds.ScaleUp},
		{1, 1, 1, 0, 0, 0, ds.ScaleDown},
		//{0, 1, 0, 0, ds.ScaleDown},
	}

	for _, entry := range table {
		readDeploymentYaml(t)
		createDeployment(t, mcmDeploy, entry.mcmReplicas)
		createDeployment(t, caDeploy, entry.caReplicas)
		createDeployment(t, kcmDeploy, entry.kcmReplicas)

		_ = entry.f(ctx)
		// g.Expect(err.Error()).To(ContainSubstring("\"" + kcmDeploy.ObjectMeta.Name + "\"" + " not found"))
		// _, err = util.GetDeploymentFor(ctx, kcmDeploy.ObjectMeta.Name, kcmDeploy.ObjectMeta.Namespace, k8sClient)
		// g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		matchReplicas(t, kcmDeploy.ObjectMeta.Namespace, kcmDeploy.ObjectMeta.Name, entry.expectedkcmReplicas)
		matchReplicas(t, caDeploy.ObjectMeta.Namespace, caDeploy.ObjectMeta.Name, entry.expectedcaReplicas)
		matchReplicas(t, mcmDeploy.ObjectMeta.Namespace, mcmDeploy.ObjectMeta.Name, entry.expectedmcmReplicas)

		deleteDeployment(t)
	}
}

func readDeploymentYaml(t *testing.T) {
	g := NewWithT(t)
	fileExistsOrFail(deploymentPath)
	result := getStructured[appsv1.Deployment](deploymentPath)
	g.Expect(result.Err).To(BeNil())
	g.Expect(result.StructuredObject).ToNot(BeNil())
	mcmDeploy = result.StructuredObject.DeepCopy()
	mcmDeploy.ObjectMeta.Name = mcmRef.Name
	kcmDeploy = result.StructuredObject.DeepCopy()
	kcmDeploy.ObjectMeta.Name = kcmRef.Name
	caDeploy = result.StructuredObject.DeepCopy()
	caDeploy.ObjectMeta.Name = caRef.Name
}

func createDeployment(t *testing.T, deploy *appsv1.Deployment, replicas int32) {
	g := NewWithT(t)
	deploy.Spec.Replicas = &replicas
	err := k8sClient.Create(ctx, deploy)
	g.Expect(err).To(BeNil())
}

func matchReplicas(t *testing.T, namespace string, name string, expectedReplicas int32) {
	g := NewWithT(t)
	deploy, err := util.GetDeploymentFor(ctx, namespace, name, k8sClient)
	g.Expect(err).To(BeNil())
	g.Expect(deploy).ToNot(BeNil())
	log.Println(name, " ", *(deploy.Spec.Replicas))
	g.Expect(*(deploy.Spec.Replicas)).Should(Equal(expectedReplicas))
}

func deleteDeployment(t *testing.T) {
	g := NewWithT(t)
	opts := []client.DeleteAllOfOption{
		client.InNamespace(namespace),
	}
	err := k8sClient.DeleteAllOf(ctx, &appsv1.Deployment{}, opts...)
	g.Expect(err).To(BeNil())
}

func createProbeConfig() {
	var dependentResourceInfos []DependentResourceInfo
	dependentResourceInfos = append(dependentResourceInfos, createDependentResourceInfo(mcmRef.Name, 2, 0, 1, 0))
	dependentResourceInfos = append(dependentResourceInfos, createDependentResourceInfo(kcmRef.Name, 1, 0, 1, 0))
	dependentResourceInfos = append(dependentResourceInfos, createDependentResourceInfo(caRef.Name, 0, 1, 1, 0))
	probeCfg = &Config{Namespace: namespace, DependentResourceInfos: dependentResourceInfos}
}
func TestSortAndGetUniqueLevels(t *testing.T) {
	g := NewWithT(t)
	numResInfosByLevel := map[int]int{2: 1, 0: 2, 1: 2}
	resInfos := createScaleableResourceInfos(numResInfosByLevel)
	levels := sortAndGetUniqueLevels(resInfos)
	g.Expect(levels).ToNot(BeNil())
	g.Expect(levels).ToNot(BeEmpty())
	g.Expect(len(levels)).To(Equal(3))
	g.Expect(levels).To(Equal([]int{0, 1, 2}))
}

func TestSortAndGetUniqueLevelsForEmptyScaleableResourceInfos(t *testing.T) {
	g := NewWithT(t)
	levels := sortAndGetUniqueLevels([]scaleableResourceInfo{})
	g.Expect(levels).To(BeNil())
}

func TestCreateScaleUpResourceInfos(t *testing.T) {
	g := NewWithT(t)
	var depResInfos []DependentResourceInfo
	depResInfos = append(depResInfos, createDependentResourceInfo(mcmRef.Name, 2, 0, 1, 0))
	depResInfos = append(depResInfos, createDependentResourceInfo(caRef.Name, 0, 1, 1, 0))
	depResInfos = append(depResInfos, createDependentResourceInfo(kcmRef.Name, 1, 0, 1, 0))

	scaleUpResInfos := createScaleUpResourceInfos(depResInfos)
	g.Expect(scaleUpResInfos).ToNot(BeNil())
	g.Expect(scaleUpResInfos).ToNot(BeEmpty())
	g.Expect(len(scaleUpResInfos)).To(Equal(len(depResInfos)))

	g.Expect(scaleableResourceMatchFound(scaleableResourceInfo{ref: mcmRef, level: 2, initialDelay: defaultInitialDelay, timeout: defaultTimeout, replicas: 1}, scaleUpResInfos)).To(BeTrue())
	g.Expect(scaleableResourceMatchFound(scaleableResourceInfo{ref: caRef, level: 0, initialDelay: defaultInitialDelay, timeout: defaultTimeout, replicas: 1}, scaleUpResInfos)).To(BeTrue())
	g.Expect(scaleableResourceMatchFound(scaleableResourceInfo{ref: kcmRef, level: 1, initialDelay: defaultInitialDelay, timeout: defaultTimeout, replicas: 1}, scaleUpResInfos)).To(BeTrue())
}

func TestCreateScaleDownResourceInfos(t *testing.T) {
	g := NewWithT(t)
	var depResInfos []DependentResourceInfo
	depResInfos = append(depResInfos, createDependentResourceInfo(mcmRef.Name, 1, 0, 1, 0))
	depResInfos = append(depResInfos, createDependentResourceInfo(caRef.Name, 0, 1, 2, 1))
	depResInfos = append(depResInfos, createDependentResourceInfo(kcmRef.Name, 1, 0, 1, 0))

	scaleDownResInfos := createScaleDownResourceInfos(depResInfos)
	g.Expect(scaleDownResInfos).ToNot(BeNil())
	g.Expect(scaleDownResInfos).ToNot(BeEmpty())
	g.Expect(len(scaleDownResInfos)).To(Equal(len(depResInfos)))

	g.Expect(scaleableResourceMatchFound(scaleableResourceInfo{ref: mcmRef, level: 0, initialDelay: defaultInitialDelay, timeout: defaultTimeout, replicas: 0}, scaleDownResInfos)).To(BeTrue())
	g.Expect(scaleableResourceMatchFound(scaleableResourceInfo{ref: caRef, level: 1, initialDelay: defaultInitialDelay, timeout: defaultTimeout, replicas: 1}, scaleDownResInfos)).To(BeTrue())
	g.Expect(scaleableResourceMatchFound(scaleableResourceInfo{ref: kcmRef, level: 0, initialDelay: defaultInitialDelay, timeout: defaultTimeout, replicas: 0}, scaleDownResInfos)).To(BeTrue())
}

// utility methods to be used by tests
//------------------------------------------------------------------------------------------------------------------
// createScaleableResourceInfos creates a slice of scaleableResourceInfo's taking in a map whose key is level
// and value is the number of scaleableResourceInfo's to be created at that level
func createScaleableResourceInfos(numResInfosByLevel map[int]int) []scaleableResourceInfo {
	var resInfos []scaleableResourceInfo
	for k, v := range numResInfosByLevel {
		for i := 0; i < v; i++ {
			resInfos = append(resInfos, scaleableResourceInfo{
				ref:   autoscalingv1.CrossVersionObjectReference{Name: fmt.Sprintf("resource-%d%d", k, i)},
				level: k,
			})
		}
	}
	return resInfos
}

func createDependentResourceInfo(name string, scaleUpLevel, scaleDownLevel int, scaleUpReplicas, scaleDownReplicas int32) DependentResourceInfo {
	return DependentResourceInfo{
		Ref: autoscalingv1.CrossVersionObjectReference{Name: name, Kind: "Deployment", APIVersion: "apps/v1"},
		ScaleUpInfo: &ScaleInfo{
			Level:        scaleUpLevel,
			InitialDelay: &defaultInitialDelay,
			Timeout:      &defaultTimeout,
			Replicas:     &scaleUpReplicas,
		},
		ScaleDownInfo: &ScaleInfo{
			Level:        scaleDownLevel,
			InitialDelay: &defaultInitialDelay,
			Timeout:      &defaultTimeout,
			Replicas:     &scaleDownReplicas,
		},
	}
}

func scaleableResourceMatchFound(expected scaleableResourceInfo, resources []scaleableResourceInfo) bool {
	for _, resInfo := range resources {
		if resInfo.ref.Name == expected.ref.Name {
			// compare all values which are not nil
			return reflect.DeepEqual(expected.ref, resInfo.ref) && expected.level == resInfo.level && expected.replicas == resInfo.replicas
		}
	}
	return false
}
