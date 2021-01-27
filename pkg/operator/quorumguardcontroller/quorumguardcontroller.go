package quorumguardcontroller

import (
	"context"
	"fmt"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	"gopkg.in/yaml.v2"
	appsv1 "k8s.io/api/apps/v1"
	policyv1beta1 "k8s.io/api/policy/v1beta1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"strconv"
	"time"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/etcd_assets"
	"github.com/openshift/library-go/pkg/operator/resource/resourceread"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	configv1 "github.com/openshift/api/config/v1"
	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/lib/resourceapply"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
)

const (
	EtcdGuardDeploymentName            = "etcd-quorum-guard"
	infrastructureClusterName          = "cluster"
	clusterConfigName                  = "cluster-config-v1"
	clusterConfigKey                   = "install-config"
	clusterConfigNamespace             = "kube-system"
	pdbDeploymentMaxUnavailableDefault = 1
)

type replicaCountDecoder struct {
	ControlPlane struct {
		Replicas string `yaml:"replicas,omitempty"`
	} `yaml:"controlPlane,omitempty"`
}

// QuorumGuardController watches the etcd quorum guard deployment, create if not exists
type QuorumGuardController struct {
	operatorClient       v1helpers.OperatorClient
	kubeClient           kubernetes.Interface
	podLister            corev1listers.PodLister
	nodeLister           corev1listers.NodeLister
	infrastructureLister configv1listers.InfrastructureLister
	clusterTopology      configv1.TopologyMode
	replicaCount         int
	etcdQuorumGuard      *appsv1.Deployment
}

func NewQuorumGuardController(
	operatorClient v1helpers.OperatorClient,
	kubeClient kubernetes.Interface,
	kubeInformers v1helpers.KubeInformersForNamespaces,
	eventRecorder events.Recorder,
	infrastructureLister configv1listers.InfrastructureLister,
) factory.Controller {
	c := &QuorumGuardController{
		operatorClient:       operatorClient,
		kubeClient:           kubeClient,
		podLister:            kubeInformers.InformersFor(operatorclient.TargetNamespace).Core().V1().Pods().Lister(),
		nodeLister:           kubeInformers.InformersFor("").Core().V1().Nodes().Lister(),
		infrastructureLister: infrastructureLister,
		replicaCount:         0,
	}
	return factory.New().ResyncEvery(time.Minute).WithInformers(
		kubeInformers.InformersFor(operatorclient.TargetNamespace).Core().V1().Pods().Informer(),
		kubeInformers.InformersFor("").Core().V1().Nodes().Informer(),
		operatorClient.Informer(),
	).WithSync(c.sync).ResyncEvery(10*time.Minute).ToController("QuorumGuardController", eventRecorder.WithComponentSuffix("quorum-guard-controller"))
}

func (c *QuorumGuardController) getTopologyMode() (configv1.TopologyMode, error) {
	// right now cluster topology cannot change, infrastructure cr is immutable,
	// so we set it once in order not to run api call each time
	if c.clusterTopology != "" {
		klog.V(4).Infof("HA mode is: %s", c.clusterTopology)
		return c.clusterTopology, nil
	}

	var err error
	c.clusterTopology, err = ceohelpers.GetControlPlaneTopology(c.infrastructureLister)
	if err != nil {
		klog.Errorf("Failed to get topology mode %w ", err)
		return "", err
	}

	klog.Infof("HA mode is: %s", c.clusterTopology)

	return c.clusterTopology, nil
}

func (c *QuorumGuardController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	err := c.ensureEtcdGuard(ctx, syncCtx.Recorder())
	if err != nil {
		_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "QuorumGuardControllerDegraded",
			Status:  operatorv1.ConditionTrue,
			Reason:  "Error",
			Message: err.Error(),
		}))
		if updateErr != nil {
			syncCtx.Recorder().Warning("QuorumGuardControllerUpdatingStatus", updateErr.Error())
		}
		return err
	}

	_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient,
		v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:   "QuorumGuardControllerDegraded",
			Status: operatorv1.ConditionFalse,
			Reason: "AsExpected",
		}))
	return updateErr
}

func (c *QuorumGuardController) ensureEtcdGuard(ctx context.Context, recorder events.Recorder) error {
	haTopologyMode, err := c.getTopologyMode()
	if err != nil {
		return err
	}

	if haTopologyMode != configv1.HighlyAvailableTopologyMode {
		return nil
	}

	replicaCount, err := c.getMastersReplicaCount(ctx)
	if err != nil {
		return err
	}

	if err := c.ensureEtcdGuardDeployment(ctx, replicaCount, recorder); err != nil {
		return err
	}

	if err := c.ensureEtcdGuardPdb(ctx, recorder); err != nil {
		return err
	}

	return nil
}

// ensureEtcdGuardDeployment if etcd quorum guard deployment doesn't exist or was changed - apply it
func (c *QuorumGuardController) ensureEtcdGuardDeployment(ctx context.Context, replicaCount int32, recorder events.Recorder) error {

	if c.etcdQuorumGuard == nil {
		c.etcdQuorumGuard = resourceread.ReadDeploymentV1OrDie(etcd_assets.MustAsset("etcd/quorumguard-deployment.yaml"))
	}
	c.etcdQuorumGuard.Spec.Replicas = &replicaCount

	// if restart occurred, we will apply etcd guard deployment but if it is the same, nothing will happened
	actual, modified, err := resourceapply.ApplyDeploymentv1(ctx, c.kubeClient.AppsV1(), c.etcdQuorumGuard)
	if err != nil {
		klog.Errorf("Failed to verify/apply %s, error %w", EtcdGuardDeploymentName, err)
		return err
	}

	// if deployment was modified need to save spec from modified one
	// cause there are some fields added on creation or apply, for example permissions on volumes
	// and we want to save them to be able to verify that deployment was not changed
	if modified {
		c.etcdQuorumGuard.Spec = actual.Spec
		msg := fmt.Sprintf("%s was modified", EtcdGuardDeploymentName)
		klog.Infof("%s was modified", EtcdGuardDeploymentName)
		recorder.Event("ModifiedEtcdGuardDeployment", msg)
	}

	return nil
}

// ensureEtcdGuardPdb if etcd quorum guard pdb doesn't exist or ws changed, apply one
func (c *QuorumGuardController) ensureEtcdGuardPdb(ctx context.Context, recorder events.Recorder) error {
	maxUnavailable := intstr.FromInt(pdbDeploymentMaxUnavailableDefault)

	pdb := &policyv1beta1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      EtcdGuardDeploymentName,
			Namespace: operatorclient.TargetNamespace,
		},
		Spec: policyv1beta1.PodDisruptionBudgetSpec{
			MaxUnavailable: &maxUnavailable,
			Selector:       &metav1.LabelSelector{MatchLabels: map[string]string{"k8s-app": EtcdGuardDeploymentName}},
		},
	}

	// if restart occurred, we will apply pdb but if it is the same, nothing will happened
	_, modified, err := resourceapply.ApplyPodDisruptionBudgets(ctx, c.kubeClient.PolicyV1beta1(), pdb)
	if err != nil {
		klog.Errorf("Failed to verify/apply %s pdb, error %w", EtcdGuardDeploymentName, err)
		return err
	}

	// if PDB was modified log and event it
	if modified {
		msg := fmt.Sprintf("%s pdb was modified", EtcdGuardDeploymentName)
		klog.Info(msg)
		recorder.Event("ModifiedEtcdGuardPdb", msg)
	}

	return nil
}

// getMastersReplicaCount get number of expected masters
func (c *QuorumGuardController) getMastersReplicaCount(ctx context.Context) (int32, error) {
	if c.replicaCount != 0 {
		return int32(c.replicaCount), nil
	}

	klog.Infof("Getting number of expected masters from %s", clusterConfigName)
	clusterConfig, err := c.kubeClient.CoreV1().ConfigMaps(clusterConfigNamespace).Get(ctx, clusterConfigName, metav1.GetOptions{})
	if err != nil {
		klog.Errorf("Failed to get ConfigMap %s, err %w", clusterConfigName, err)
		return 0, err
	}

	rcD := replicaCountDecoder{}
	if err := yaml.Unmarshal([]byte(clusterConfig.Data[clusterConfigKey]), &rcD); err != nil {
		err := fmt.Errorf("%s key doesn't exist in ConfigMap %s", clusterConfigKey, clusterConfigName)
		klog.Error(err)
		return 0, err
	}

	c.replicaCount, err = strconv.Atoi(rcD.ControlPlane.Replicas)
	if err != nil {
		klog.Errorf("failed to convert replica %s, err %v", rcD.ControlPlane.Replicas, err)
		return 0, err
	}
	return int32(c.replicaCount), nil
}
