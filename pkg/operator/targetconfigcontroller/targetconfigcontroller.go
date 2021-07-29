package targetconfigcontroller

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	coreclientv1 "k8s.io/client-go/kubernetes/typed/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	configv1 "github.com/openshift/api/config/v1"
	operatorv1 "github.com/openshift/api/operator/v1"
	configv1informers "github.com/openshift/client-go/config/informers/externalversions/config/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/resource/resourceread"
	"github.com/openshift/library-go/pkg/operator/v1helpers"

	"github.com/openshift/cluster-etcd-operator/pkg/etcdenvvar"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/etcd_assets"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-etcd-operator/pkg/version"
)

type TargetConfigController struct {
	targetImagePullSpec   string
	operatorImagePullSpec string

	operatorClient v1helpers.StaticPodOperatorClient

	kubeClient           kubernetes.Interface
	infrastructureLister configv1listers.InfrastructureLister
	networkLister        configv1listers.NetworkLister
	configMapLister      corev1listers.ConfigMapLister
	endpointLister       corev1listers.EndpointsLister
	nodeLister           corev1listers.NodeLister
	envVarGetter         *etcdenvvar.EnvVarController

	enqueueFn func()
}

func NewTargetConfigController(
	targetImagePullSpec, operatorImagePullSpec string,
	operatorClient v1helpers.StaticPodOperatorClient,
	kubeInformersForOpenshiftEtcdNamespace informers.SharedInformerFactory,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
	infrastructureInformer configv1informers.InfrastructureInformer,
	networkInformer configv1informers.NetworkInformer,
	kubeClient kubernetes.Interface,
	envVarGetter *etcdenvvar.EnvVarController,
	eventRecorder events.Recorder,
) factory.Controller {
	c := &TargetConfigController{
		targetImagePullSpec:   targetImagePullSpec,
		operatorImagePullSpec: operatorImagePullSpec,

		operatorClient:       operatorClient,
		kubeClient:           kubeClient,
		infrastructureLister: infrastructureInformer.Lister(),
		networkLister:        networkInformer.Lister(),
		configMapLister:      kubeInformersForNamespaces.ConfigMapLister(),
		endpointLister:       kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().Endpoints().Lister(),
		nodeLister:           kubeInformersForNamespaces.InformersFor("").Core().V1().Nodes().Lister(),
		envVarGetter:         envVarGetter,
	}

	syncCtx := factory.NewSyncContext("TargetConfigController", eventRecorder.WithComponentSuffix("target-config-controller"))
	c.enqueueFn = func() {
		syncCtx.Queue().Add(syncCtx.QueueKey())
	}
	envVarGetter.AddListener(c)

	return factory.New().WithSyncContext(syncCtx).ResyncEvery(time.Minute).WithInformers(
		operatorClient.Informer(),
		kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().Endpoints().Informer(),
		kubeInformersForOpenshiftEtcdNamespace.Core().V1().ConfigMaps().Informer(),
		kubeInformersForOpenshiftEtcdNamespace.Core().V1().Secrets().Informer(),
		kubeInformersForNamespaces.InformersFor("").Core().V1().Nodes().Informer(),
		infrastructureInformer.Informer(),
		networkInformer.Informer(),
	).WithSync(c.sync).ToController("TargetConfigController", syncCtx.Recorder())
}

func (c TargetConfigController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	operatorSpec, _, _, err := c.operatorClient.GetStaticPodOperatorState()
	if err != nil {
		return err
	}
	requeue, err := createTargetConfig(ctx, c, syncCtx.Recorder(), operatorSpec)
	if err != nil {
		return err
	}
	if requeue {
		return fmt.Errorf("synthetic requeue request")
	}

	return nil
}

// createTargetConfig takes care of creation of valid resources in a fixed name.  These are inputs to other control loops.
// returns whether or not requeue and if an error happened when updating status.  Normally it updates status itself.
func createTargetConfig(ctx context.Context, c TargetConfigController, recorder events.Recorder, operatorSpec *operatorv1.StaticPodOperatorSpec) (bool, error) {
	errors := []error{}

	contentReplacer, err := c.getSubstitutionReplacer(operatorSpec, c.targetImagePullSpec, c.operatorImagePullSpec)
	if err != nil {
		return false, err
	}
	if err := checkExternalDependencies(ctx, c.configMapLister, c.infrastructureLister, recorder); err != nil {
		recorder.Warning("DependencyCheckFailure", err.Error())
		return false, err
	}
	_, _, err = c.manageStandardPod(ctx, contentReplacer, c.kubeClient.CoreV1(), recorder, operatorSpec)
	if err != nil {
		errors = append(errors, fmt.Errorf("%q: %v", "configmap/etcd-pod", err))
	}
	_, _, err = c.manageRecoveryPod(ctx, contentReplacer, c.kubeClient.CoreV1(), recorder, operatorSpec)
	if err != nil {
		errors = append(errors, fmt.Errorf("%q: %v", "configmap/restore-etcd-pod", err))
	}

	if len(errors) > 0 {
		condition := operatorv1.OperatorCondition{
			Type:    "TargetConfigControllerDegraded",
			Status:  operatorv1.ConditionTrue,
			Reason:  "SynchronizationError",
			Message: v1helpers.NewMultiLineAggregate(errors).Error(),
		}
		if _, _, err := v1helpers.UpdateStaticPodStatus(c.operatorClient, v1helpers.UpdateStaticPodConditionFn(condition)); err != nil {
			return true, err
		}
		return true, nil
	}

	condition := operatorv1.OperatorCondition{
		Type:   "TargetConfigControllerDegraded",
		Status: operatorv1.ConditionFalse,
	}
	if _, _, err := v1helpers.UpdateStaticPodStatus(c.operatorClient, v1helpers.UpdateStaticPodConditionFn(condition)); err != nil {
		return true, err
	}

	return false, nil
}

func loglevelToZap(logLevel operatorv1.LogLevel) string {
	switch logLevel {
	case operatorv1.Debug, operatorv1.Trace, operatorv1.TraceAll:
		return "debug"
	default:
		return "info"
	}
}

func (c *TargetConfigController) getSubstitutionReplacer(operatorSpec *operatorv1.StaticPodOperatorSpec, imagePullSpec, operatorImagePullSpec string) (*strings.Replacer, error) {
	envVarMap := c.envVarGetter.GetEnvVars()
	if len(envVarMap) == 0 {
		return nil, fmt.Errorf("missing env var values")
	}

	envVarLines := []string{}
	for _, k := range sets.StringKeySet(envVarMap).List() {
		v := envVarMap[k]
		envVarLines = append(envVarLines, fmt.Sprintf("      - name: %q", k))
		envVarLines = append(envVarLines, fmt.Sprintf("        value: %q", v))
	}

	return strings.NewReplacer(
		"${IMAGE}", imagePullSpec,
		"${OPERATOR_IMAGE}", operatorImagePullSpec,
		"${VERBOSITY}", loglevelToZap(operatorSpec.LogLevel),
		"${LISTEN_ON_ALL_IPS}", "0.0.0.0", // TODO this needs updating to detect ipv6-ness
		"${LOCALHOST_IP}", "127.0.0.1", // TODO this needs updating to detect ipv6-ness
		"${COMPUTED_ENV_VARS}", strings.Join(envVarLines, "\n"), // lacks beauty, but it works
	), nil
}

func (c *TargetConfigController) manageRecoveryPod(ctx context.Context, substitutionReplacer *strings.Replacer, client coreclientv1.ConfigMapsGetter, recorder events.Recorder, operatorSpec *operatorv1.StaticPodOperatorSpec) (*corev1.ConfigMap, bool, error) {
	podBytes := etcd_assets.MustAsset("etcd/restore-pod.yaml")
	substitutedPodString := substitutionReplacer.Replace(string(podBytes))

	podConfigMap := resourceread.ReadConfigMapV1OrDie(etcd_assets.MustAsset("etcd/restore-pod-cm.yaml"))
	podConfigMap.Data["pod.yaml"] = substitutedPodString
	podConfigMap.Data["forceRedeploymentReason"] = operatorSpec.ForceRedeploymentReason
	podConfigMap.Data["version"] = version.Get().String()
	return resourceapply.ApplyConfigMap(ctx, client, recorder, podConfigMap)
}

func (c *TargetConfigController) manageStandardPod(ctx context.Context, substitutionReplacer *strings.Replacer, client coreclientv1.ConfigMapsGetter, recorder events.Recorder, operatorSpec *operatorv1.StaticPodOperatorSpec) (*corev1.ConfigMap, bool, error) {
	podBytes := etcd_assets.MustAsset("etcd/pod.yaml")
	substitutedPodString := substitutionReplacer.Replace(string(podBytes))

	podConfigMap := resourceread.ReadConfigMapV1OrDie(etcd_assets.MustAsset("etcd/pod-cm.yaml"))
	podConfigMap.Data["pod.yaml"] = substitutedPodString
	podConfigMap.Data["forceRedeploymentReason"] = operatorSpec.ForceRedeploymentReason
	podConfigMap.Data["version"] = version.Get().String()
	return resourceapply.ApplyConfigMap(ctx, client, recorder, podConfigMap)
}

func (c *TargetConfigController) Enqueue() {
	c.enqueueFn()
}

func (c *TargetConfigController) namespaceEventHandler() cache.ResourceEventHandler {
	return cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			ns, ok := obj.(*corev1.Namespace)
			if !ok {
				c.Enqueue()
			}
			if ns.Name == ("openshift-etcd") {
				c.Enqueue()
			}
		},
		UpdateFunc: func(old, new interface{}) {
			ns, ok := old.(*corev1.Namespace)
			if !ok {
				c.Enqueue()
			}
			if ns.Name == ("openshift-etcd") {
				c.Enqueue()
			}
		},
		DeleteFunc: func(obj interface{}) {
			ns, ok := obj.(*corev1.Namespace)
			if !ok {
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					utilruntime.HandleError(fmt.Errorf("couldn't get object from tombstone %#v", obj))
					return
				}
				ns, ok = tombstone.Obj.(*corev1.Namespace)
				if !ok {
					utilruntime.HandleError(fmt.Errorf("tombstone contained object that is not a Namespace %#v", obj))
					return
				}
			}
			if ns.Name == ("openshift-etcd") {
				c.Enqueue()
			}
		},
	}
}

// checkExternalDependencies ensures that resources critical to cluster stability are valid before possible disruptive rollout.
func checkExternalDependencies(ctx context.Context, lister corev1listers.ConfigMapLister, infrastructureLister configv1listers.InfrastructureLister, recorder events.Recorder) error {
	infra, err := infrastructureLister.Get("cluster")
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if infra.Status.ControlPlaneTopology == configv1.SingleReplicaTopologyMode {
		csrControllerCAConfigMap, err := lister.ConfigMaps(operatorclient.GlobalMachineSpecifiedConfigNamespace).Get("csr-controller-ca")
		if err != nil {
			return err
		}
		if err := checkCSRControllerCAConfigMap(csrControllerCAConfigMap); err != nil {
			return err
		}
	}
	return nil
}

// checkCSRControllerCA validates that the openshift-config-managed configmap csr-controller-ca contains a
// CA generated by kube-controller-manager-operator.
func checkCSRControllerCAConfigMap(csrControllerCAConfigMap *corev1.ConfigMap) error {
	var isCAManagerExpected bool
	for _, managedField := range csrControllerCAConfigMap.ManagedFields {
		if managedField.Manager == "cluster-kube-controller-manager-operator" {
			isCAManagerExpected = true
		}
	}

	if !isCAManagerExpected {
		return fmt.Errorf("configmap openshift-config-managed/csr-controller-ca field manager is not valid")
	}

	return nil
}
