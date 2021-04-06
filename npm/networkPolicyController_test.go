// Copyright 2018 Microsoft. All rights reserved.
// MIT License
package npm

import (
	"testing"

	"github.com/Azure/azure-container-networking/npm/ipsm"
	"github.com/Azure/azure-container-networking/npm/iptm"
	"github.com/Azure/azure-container-networking/npm/metrics"
	"github.com/Azure/azure-container-networking/npm/metrics/promutil"
	"github.com/Azure/azure-container-networking/npm/util"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	kubeinformers "k8s.io/client-go/informers"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	core "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
)

type netPolFixture struct {
	t *testing.T

	kubeclient *k8sfake.Clientset
	// Objects to put in the store.
	netPolLister []*networkingv1.NetworkPolicy
	// (TODO) Actions expected to happen on the client. Will use this to check action.
	kubeactions []core.Action
	// Objects from here preloaded into NewSimpleFake.
	kubeobjects []runtime.Object

	// (TODO) will remove npMgr if possible
	npMgr            *NetworkPolicyManager
	ipsMgr           *ipsm.IpsetManager
	netPolController *networkPolicyController
	kubeInformer     kubeinformers.SharedInformerFactory
}

func newNetPolFixture(t *testing.T) *netPolFixture {
	f := &netPolFixture{
		t:            t,
		netPolLister: []*networkingv1.NetworkPolicy{},
		kubeobjects:  []runtime.Object{},
		npMgr:        newNPMgr(t),
		ipsMgr:       ipsm.NewIpsetManager(),
	}

	f.npMgr.RawNpMap = make(map[string]*networkingv1.NetworkPolicy)
	f.npMgr.ProcessedNpMap = make(map[string]*networkingv1.NetworkPolicy)
	return f
}

func (f *netPolFixture) newNetPodController(stopCh chan struct{}) {
	f.kubeclient = k8sfake.NewSimpleClientset(f.kubeobjects...)
	f.kubeInformer = kubeinformers.NewSharedInformerFactory(f.kubeclient, noResyncPeriodFunc())

	f.netPolController = NewNetworkPolicyController(f.kubeInformer.Networking().V1().NetworkPolicies(), f.kubeclient, f.npMgr)
	f.netPolController.netPolListerSynced = alwaysReady

	for _, netPol := range f.netPolLister {
		f.kubeInformer.Networking().V1().NetworkPolicies().Informer().GetIndexer().Add(netPol)
	}

	f.kubeInformer.Start(stopCh)
}

func (f *netPolFixture) ipSetSave(ipsetConfigFile string) {
	//  call /sbin/ipset save -file /var/log/ipset-test.conf
	f.t.Logf("Start storing ipset to %s", ipsetConfigFile)
	if err := f.ipsMgr.Save(ipsetConfigFile); err != nil {
		f.t.Errorf("ipSetSave failed @ ipsMgr.Save")
	}
}
func (f *netPolFixture) ipSetRestore(ipsetConfigFile string) {
	//  call /sbin/ipset restore -file /var/log/ipset-test.conf
	f.t.Logf("Start re-storing ipset to %s", ipsetConfigFile)
	if err := f.ipsMgr.Restore(ipsetConfigFile); err != nil {
		f.t.Errorf("ipSetRestore failed @ ipsMgr.Restore")
	}
}

// (TODO): fix it
func createNetPol() *networkingv1.NetworkPolicy {
	tcp := corev1.ProtocolTCP
	port8000 := intstr.FromInt(8000)
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "allow-ingress",
			Namespace: "test-nwpolicy",
		},
		Spec: networkingv1.NetworkPolicySpec{
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				networkingv1.NetworkPolicyIngressRule{
					From: []networkingv1.NetworkPolicyPeer{
						networkingv1.NetworkPolicyPeer{
							PodSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{"app": "test"},
							},
						},
						networkingv1.NetworkPolicyPeer{
							IPBlock: &networkingv1.IPBlock{
								CIDR: "0.0.0.0/0",
							},
						},
					},
					Ports: []networkingv1.NetworkPolicyPort{{
						Protocol: &tcp,
						Port:     &port8000,
					}},
				},
			},
		},
	}
}

func getNetPolKey(netPolObj *networkingv1.NetworkPolicy, t *testing.T) string {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(netPolObj)
	if err != nil {
		t.Errorf("Unexpected error getting key for network policy %s: %v", netPolObj.Name, err)
		return ""
	}
	return key
}

func addNetPol(t *testing.T, f *netPolFixture, netPolObj *networkingv1.NetworkPolicy) {
	// simulate "network policy" add event and add network policy object to sharedInformer cache
	f.netPolController.addNetPol(netPolObj)

	if f.netPolController.workqueue.Len() == 0 {
		t.Logf("Add network policy: worker queue length is 0 ")
		return
	}

	f.netPolController.processNextWorkItem()
}

func deleteNetPod(t *testing.T, f *netPolFixture, netPolObj *networkingv1.NetworkPolicy) {
	addNetPol(t, f, netPolObj)
	t.Logf("Complete adding network policy event")

	// simulate network policy deletion event and delete network policy object from sharedInformer cache
	f.kubeInformer.Networking().V1().NetworkPolicies().Informer().GetIndexer().Delete(netPolObj)
	f.netPolController.deleteNetPol(netPolObj)

	if f.netPolController.workqueue.Len() == 0 {
		t.Logf("Delete network policy: worker queue length is 0 ")
		return
	}

	f.netPolController.processNextWorkItem()
}

// Need to make more cases - interestings..
func updateNetPol(t *testing.T, f *netPolFixture, oldNetPolObj, netNetPolObj *networkingv1.NetworkPolicy) {
	addNetPol(t, f, oldNetPolObj)
	t.Logf("Complete updating network policy event")

	// simulate pod update event and update the pod to shared informer's cache
	f.kubeInformer.Networking().V1().NetworkPolicies().Informer().GetIndexer().Update(netNetPolObj)
	f.netPolController.updateNetPol(oldNetPolObj, netNetPolObj)

	if f.netPolController.workqueue.Len() == 0 {
		t.Logf("Update Network Policy: worker queue length is 0 ")
		return
	}

	f.netPolController.processNextWorkItem()
}

// (TODO) - fix : it should be "RawNetPol" && "ProcessedNetPol"
type expectedNetPolValues struct {
	expectedLenOfNsMap                   int
	expectedLenOfRawNpMap                int
	expectedLenOfProcessedNpMap          int
	expectedIsSafeToCleanUpAzureNpmChain bool
	expectedIsAzureNpmChainCreated       bool
	expectedLenOfWorkQueue               int
}

func checkNetPolTestResult(testName string, f *netPolFixture, testCases []expectedNetPolValues) {
	for _, test := range testCases {
		if got := len(f.npMgr.NsMap); got != test.expectedLenOfNsMap {
			f.t.Errorf("npMgr namespace map length = %d, want %d", got, test.expectedLenOfNsMap)
		}

		if got := len(f.netPolController.npMgr.RawNpMap); got != test.expectedLenOfRawNpMap {
			f.t.Errorf("Raw NetPol Map length = %d, want %d", got, test.expectedLenOfRawNpMap)
		}

		if got := len(f.netPolController.npMgr.ProcessedNpMap); got != test.expectedLenOfProcessedNpMap {
			f.t.Errorf("Processed NetPol Map length = %d, want %d", got, test.expectedLenOfProcessedNpMap)
		}

		// if got := f.netPolController.isSafeToCleanUpAzureNpmChain; got != test.expectedIsSafeToCleanUpAzureNpmChain {
		// 	f.t.Errorf("isSafeToCleanUpAzureNpmChain %v, want %v", got, test.expectedLenOfRawNpMap)
		// }

		if got := f.netPolController.isAzureNpmChainCreated; got != test.expectedIsAzureNpmChainCreated {
			f.t.Errorf("isAzureNpmChainCreated %v, want %v", got, test.expectedIsAzureNpmChainCreated)

		}

		if got := f.netPolController.workqueue.Len(); got != test.expectedLenOfWorkQueue {
			f.t.Errorf("Workqueue length = %d, want %d", got, test.expectedLenOfWorkQueue)
		}
	}
}

func TestAddNetworkPolicy(t *testing.T) {
	netPolObj := createNetPol()

	f := newNetPolFixture(t)
	f.netPolLister = append(f.netPolLister, netPolObj)
	f.kubeobjects = append(f.kubeobjects, netPolObj)
	stopCh := make(chan struct{})
	defer close(stopCh)
	f.newNetPodController(stopCh)

	addNetPol(t, f, netPolObj)
	// (TODO): Why? networkPolicyController_test.go:187: npMgr namespace map length = 1, want 2
	testCases := []expectedNetPolValues{
		{2, 1, 0, false, true, 0},
	}
	checkNetPolTestResult("TestAddNetPol", f, testCases)
}

func TestDeleteNetworkPolicy(t *testing.T) {
	netPolObj := createNetPol()

	f := newNetPolFixture(t)
	f.netPolLister = append(f.netPolLister, netPolObj)
	f.kubeobjects = append(f.kubeobjects, netPolObj)
	stopCh := make(chan struct{})
	defer close(stopCh)
	f.newNetPodController(stopCh)

	deleteNetPod(t, f, netPolObj)
	// (TODO): check ground-truth value
	testCases := []expectedNetPolValues{
		{1, 0, 0, false, false, 0},
	}
	checkNetPolTestResult("TestDelNetPol", f, testCases)
}

func TestAddNetworkPolicy1(t *testing.T) {
	npMgr := &NetworkPolicyManager{
		NsMap:            make(map[string]*Namespace),
		PodMap:           make(map[string]*NpmPod),
		RawNpMap:         make(map[string]*networkingv1.NetworkPolicy),
		ProcessedNpMap:   make(map[string]*networkingv1.NetworkPolicy),
		TelemetryEnabled: false,
	}

	allNs, err := newNs(util.KubeAllNamespacesFlag)
	if err != nil {
		panic(err.Error)
	}
	npMgr.NsMap[util.KubeAllNamespacesFlag] = allNs

	iptMgr := iptm.NewIptablesManager()
	if err := iptMgr.Save(util.IptablesTestConfigFile); err != nil {
		t.Errorf("TestAddNetworkPolicy failed @ iptMgr.Save")
	}

	ipsMgr := ipsm.NewIpsetManager()
	if err := ipsMgr.Save(util.IpsetTestConfigFile); err != nil {
		t.Errorf("TestAddNetworkPolicy failed @ ipsMgr.Save")
	}

	// Create ns-kube-system set
	if err := ipsMgr.CreateSet("ns-"+util.KubeSystemFlag, append([]string{util.IpsetNetHashFlag})); err != nil {
		t.Errorf("TestAddNetworkPolicy failed @ ipsMgr.CreateSet, adding kube-system set%+v", err)
	}

	defer func() {
		if err := iptMgr.Restore(util.IptablesTestConfigFile); err != nil {
			t.Errorf("TestAddNetworkPolicy failed @ iptMgr.Restore")
		}

		if err := ipsMgr.Restore(util.IpsetTestConfigFile); err != nil {
			t.Errorf("TestAddNetworkPolicy failed @ ipsMgr.Restore")
		}
	}()

	tcp := corev1.ProtocolTCP
	port8000 := intstr.FromInt(8000)
	allowIngress := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "allow-ingress",
			Namespace: "test-nwpolicy",
		},
		Spec: networkingv1.NetworkPolicySpec{
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				networkingv1.NetworkPolicyIngressRule{
					From: []networkingv1.NetworkPolicyPeer{
						networkingv1.NetworkPolicyPeer{
							PodSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{"app": "test"},
							},
						},
						networkingv1.NetworkPolicyPeer{
							IPBlock: &networkingv1.IPBlock{
								CIDR: "0.0.0.0/0",
							},
						},
					},
					Ports: []networkingv1.NetworkPolicyPort{{
						Protocol: &tcp,
						Port:     &port8000,
					}},
				},
			},
		},
	}

	gaugeVal, err1 := promutil.GetValue(metrics.NumPolicies)
	countVal, err2 := promutil.GetCountValue(metrics.AddPolicyExecTime)

	npMgr.Lock()
	if err := npMgr.AddNetworkPolicy(allowIngress); err != nil {
		t.Errorf("TestAddNetworkPolicy failed @ allowIngress AddNetworkPolicy")
		t.Errorf("Error: %v", err)
	}
	npMgr.Unlock()

	ipsMgr = npMgr.NsMap[util.KubeAllNamespacesFlag].IpsMgr

	// Check whether 0.0.0.0/0 got translated to 1.0.0.0/1 and 128.0.0.0/1
	if !ipsMgr.Exists("allow-ingress-in-ns-test-nwpolicy-0in", "1.0.0.0/1", util.IpsetNetHashFlag) {
		t.Errorf("TestDeleteFromSet failed @ ipsMgr.AddToSet")
	}

	if !ipsMgr.Exists("allow-ingress-in-ns-test-nwpolicy-0in", "128.0.0.0/1", util.IpsetNetHashFlag) {
		t.Errorf("TestDeleteFromSet failed @ ipsMgr.AddToSet")
	}

	allowEgress := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "allow-egress",
			Namespace: "test-nwpolicy",
		},
		Spec: networkingv1.NetworkPolicySpec{
			Egress: []networkingv1.NetworkPolicyEgressRule{
				networkingv1.NetworkPolicyEgressRule{
					To: []networkingv1.NetworkPolicyPeer{{
						PodSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"app": "test"},
						},
					}},
					Ports: []networkingv1.NetworkPolicyPort{{
						Protocol: &tcp,
						Port:     &port8000,
					}},
				},
			},
		},
	}

	npMgr.Lock()
	if err := npMgr.AddNetworkPolicy(allowEgress); err != nil {
		t.Errorf("TestAddNetworkPolicy failed @ allowEgress AddNetworkPolicy")
		t.Errorf("Error: %v", err)
	}
	npMgr.Unlock()

	newGaugeVal, err3 := promutil.GetValue(metrics.NumPolicies)
	newCountVal, err4 := promutil.GetCountValue(metrics.AddPolicyExecTime)
	promutil.NotifyIfErrors(t, err1, err2, err3, err4)
	if newGaugeVal != gaugeVal+2 {
		t.Errorf("Change in policy number didn't register in prometheus")
	}
	if newCountVal != countVal+2 {
		t.Errorf("Execution time didn't register in prometheus")
	}
}

func TestUpdateNetworkPolicy(t *testing.T) {
	npMgr := &NetworkPolicyManager{
		NsMap:            make(map[string]*Namespace),
		PodMap:           make(map[string]*NpmPod),
		RawNpMap:         make(map[string]*networkingv1.NetworkPolicy),
		ProcessedNpMap:   make(map[string]*networkingv1.NetworkPolicy),
		TelemetryEnabled: false,
	}

	allNs, err := newNs(util.KubeAllNamespacesFlag)
	if err != nil {
		panic(err.Error)
	}
	npMgr.NsMap[util.KubeAllNamespacesFlag] = allNs

	iptMgr := iptm.NewIptablesManager()
	if err := iptMgr.Save(util.IptablesTestConfigFile); err != nil {
		t.Errorf("TestUpdateNetworkPolicy failed @ iptMgr.Save")
	}

	ipsMgr := ipsm.NewIpsetManager()
	if err := ipsMgr.Save(util.IpsetTestConfigFile); err != nil {
		t.Errorf("TestUpdateNetworkPolicy failed @ ipsMgr.Save")
	}

	defer func() {
		if err := iptMgr.Restore(util.IptablesTestConfigFile); err != nil {
			t.Errorf("TestUpdateNetworkPolicy failed @ iptMgr.Restore")
		}

		if err := ipsMgr.Restore(util.IpsetTestConfigFile); err != nil {
			t.Errorf("TestUpdateNetworkPolicy failed @ ipsMgr.Restore")
		}
	}()

	// Create ns-kube-system set
	if err := ipsMgr.CreateSet("ns-"+util.KubeSystemFlag, append([]string{util.IpsetNetHashFlag})); err != nil {
		t.Errorf("TestUpdateNetworkPolicy failed @ ipsMgr.CreateSet, adding kube-system set%+v", err)
	}

	tcp, udp := corev1.ProtocolTCP, corev1.ProtocolUDP
	allowIngress := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "allow-ingress",
			Namespace: "test-nwpolicy",
		},
		Spec: networkingv1.NetworkPolicySpec{
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				networkingv1.NetworkPolicyIngressRule{
					From: []networkingv1.NetworkPolicyPeer{{
						PodSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"app": "test"},
						},
					}},
					Ports: []networkingv1.NetworkPolicyPort{{
						Protocol: &tcp,
						Port: &intstr.IntOrString{
							StrVal: "8000",
						},
					}},
				},
			},
		},
	}

	allowEgress := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "allow-egress",
			Namespace: "test-nwpolicy",
		},
		Spec: networkingv1.NetworkPolicySpec{
			Egress: []networkingv1.NetworkPolicyEgressRule{
				networkingv1.NetworkPolicyEgressRule{
					To: []networkingv1.NetworkPolicyPeer{{
						NamespaceSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"ns": "test"},
						},
					}},
					Ports: []networkingv1.NetworkPolicyPort{{
						Protocol: &udp,
						Port: &intstr.IntOrString{
							StrVal: "8001",
						},
					}},
				},
			},
		},
	}

	npMgr.Lock()
	if err := npMgr.AddNetworkPolicy(allowIngress); err != nil {
		t.Errorf("TestUpdateNetworkPolicy failed @ AddNetworkPolicy")
	}

	if err := npMgr.UpdateNetworkPolicy(allowIngress, allowEgress); err != nil {
		t.Errorf("TestUpdateNetworkPolicy failed @ UpdateNetworkPolicy")
	}
	npMgr.Unlock()
}

func TestGetNetworkPolicyKey(t *testing.T) {
	// npObj := &networkingv1.NetworkPolicy{
	// 	ObjectMeta: metav1.ObjectMeta{
	// 		Name:      "allow-egress",
	// 		Namespace: "test-nwpolicy",
	// 	},
	// 	Spec: networkingv1.NetworkPolicySpec{
	// 		Egress: []networkingv1.NetworkPolicyEgressRule{
	// 			networkingv1.NetworkPolicyEgressRule{
	// 				To: []networkingv1.NetworkPolicyPeer{{
	// 					NamespaceSelector: &metav1.LabelSelector{
	// 						MatchLabels: map[string]string{"ns": "test"},
	// 					},
	// 				}},
	// 			},
	// 		},
	// 	},
	// }

	// netpolKey := GetNetworkPolicyKey(npObj)

	// if netpolKey == "" {
	// 	t.Errorf("TestGetNetworkPolicyKey failed @ netpolKey length check %s", netpolKey)
	// }

	// expectedKey := util.GetNSNameWithPrefix("test-nwpolicy/allow-egress")
	// if netpolKey != expectedKey {
	// 	t.Errorf("TestGetNetworkPolicyKey failed @ netpolKey did not match expected value %s", netpolKey)
	// }
}
