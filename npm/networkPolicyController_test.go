// Copyright 2018 Microsoft. All rights reserved.
// MIT License
package npm

import (
	"fmt"
	"testing"

	"github.com/Azure/azure-container-networking/npm/ipsm"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	kubeinformers "k8s.io/client-go/informers"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	core "k8s.io/client-go/testing"
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
	return f
}

func (f *netPolFixture) newNetPolController(stopCh chan struct{}) {
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

// (TODO): make createNetPol flexible
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

func deleteNetPol(t *testing.T, f *netPolFixture, netPolObj *networkingv1.NetworkPolicy) {
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

func updateNetPol(t *testing.T, f *netPolFixture, oldNetPolObj, netNetPolObj *networkingv1.NetworkPolicy) {
	addNetPol(t, f, oldNetPolObj)
	t.Logf("Complete adding network policy event")

	// simulate network policy update event and update the network policy to shared informer's cache
	f.kubeInformer.Networking().V1().NetworkPolicies().Informer().GetIndexer().Update(netNetPolObj)
	f.netPolController.updateNetPol(oldNetPolObj, netNetPolObj)

	if f.netPolController.workqueue.Len() == 0 {
		t.Logf("Update Network Policy: worker queue length is 0 ")
		return
	}

	f.netPolController.processNextWorkItem()
}

type expectedNetPolValues struct {
	expectedLenOfNsMap             int
	expectedLenOfRawNpMap          int
	expectedLenOfWorkQueue         int
	expectedIsAzureNpmChainCreated bool
}

func checkNetPolTestResult(testName string, f *netPolFixture, testCases []expectedNetPolValues) {
	for _, test := range testCases {
		if got := len(f.npMgr.NsMap); got != test.expectedLenOfNsMap {
			f.t.Errorf("npMgr namespace map length = %d, want %d", got, test.expectedLenOfNsMap)
		}

		if got := len(f.netPolController.npMgr.RawNpMap); got != test.expectedLenOfRawNpMap {
			f.t.Errorf("Raw NetPol Map length = %d, want %d", got, test.expectedLenOfRawNpMap)
		}

		if got := f.netPolController.isAzureNpmChainCreated; got != test.expectedIsAzureNpmChainCreated {
			f.t.Errorf("isAzureNpmChainCreated %v, want %v", got, test.expectedIsAzureNpmChainCreated)
		}

		if got := f.netPolController.workqueue.Len(); got != test.expectedLenOfWorkQueue {
			f.t.Errorf("Workqueue length = %d, want %d", got, test.expectedLenOfWorkQueue)
		}
	}
}

func TestAddMultipleNetworkPolicies(t *testing.T) {
	netPolObj1 := createNetPol()

	// deep copy netPolObj1 and change namespace and name since current createNetPol is not flexble.
	netPolObj2 := netPolObj1.DeepCopy()
	netPolObj2.Namespace = fmt.Sprintf("%s-new", netPolObj1.Namespace)
	netPolObj2.Name = fmt.Sprintf("%s-new", netPolObj1.Name)

	f := newNetPolFixture(t)
	f.netPolLister = append(f.netPolLister, netPolObj1, netPolObj2)
	f.kubeobjects = append(f.kubeobjects, netPolObj1, netPolObj2)
	stopCh := make(chan struct{})
	defer close(stopCh)
	f.newNetPolController(stopCh)

	addNetPol(t, f, netPolObj1)
	addNetPol(t, f, netPolObj2)

	testCases := []expectedNetPolValues{
		{1, 2, 0, true},
	}
	checkNetPolTestResult("TestAddMultipleNetPols", f, testCases)
}
func TestAddNetworkPolicy(t *testing.T) {
	netPolObj := createNetPol()

	f := newNetPolFixture(t)
	f.netPolLister = append(f.netPolLister, netPolObj)
	f.kubeobjects = append(f.kubeobjects, netPolObj)
	stopCh := make(chan struct{})
	defer close(stopCh)
	f.newNetPolController(stopCh)

	addNetPol(t, f, netPolObj)
	testCases := []expectedNetPolValues{
		{1, 1, 0, true},
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
	f.newNetPolController(stopCh)

	deleteNetPol(t, f, netPolObj)
	testCases := []expectedNetPolValues{
		{1, 0, 0, false},
	}
	checkNetPolTestResult("TestDelNetPol", f, testCases)
}

// Need to make more test cases
func TestUpdateNetworkPolicy(t *testing.T) {
	oldNetPolObj := createNetPol()

	f := newNetPolFixture(t)
	f.netPolLister = append(f.netPolLister, oldNetPolObj)
	f.kubeobjects = append(f.kubeobjects, oldNetPolObj)
	stopCh := make(chan struct{})
	defer close(stopCh)
	f.newNetPolController(stopCh)

	newNetPolObj := oldNetPolObj.DeepCopy()
	// update podSelctor in a new network policy field
	newNetPolObj.Spec.PodSelector = metav1.LabelSelector{
		MatchLabels: map[string]string{
			"app": "test",
			"new": "test",
		},
	}

	updateNetPol(t, f, oldNetPolObj, newNetPolObj)
	testCases := []expectedNetPolValues{
		{1, 1, 0, true},
	}
	checkNetPolTestResult("TestUpdateNetPol", f, testCases)
}
