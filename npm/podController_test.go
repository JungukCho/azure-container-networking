// Copyright 2018 Microsoft. All rights reserved.
// MIT License
package npm

import (
	"reflect"
	"testing"
	"time"

	"github.com/Azure/azure-container-networking/npm/ipsm"
	"github.com/Azure/azure-container-networking/npm/util"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubeinformers "k8s.io/client-go/informers"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	core "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
)

var (
	alwaysReady        = func() bool { return true }
	noResyncPeriodFunc = func() time.Duration { return 0 }
)

type podFixture struct {
	t *testing.T

	kubeclient *k8sfake.Clientset
	// Objects to put in the store.
	podLister []*corev1.Pod
	// Actions expected to happen on the client.
	kubeactions []core.Action
	// Objects from here preloaded into NewSimpleFake.
	kubeobjects []runtime.Object

	// (TODO) will remove npMgr if possible
	npMgr         *NetworkPolicyManager
	ipsMgr        *ipsm.IpsetManager
	podController *podController
	kubeInformer  kubeinformers.SharedInformerFactory
}

func newFixture(t *testing.T) *podFixture {
	f := &podFixture{
		t:           t,
		podLister:   []*corev1.Pod{},
		kubeobjects: []runtime.Object{},
		npMgr:       newNPMgr(t),
		ipsMgr:      ipsm.NewIpsetManager(),
	}
	return f
}

func (f *podFixture) newPodController() {
	f.kubeclient = k8sfake.NewSimpleClientset(f.kubeobjects...)
	f.kubeInformer = kubeinformers.NewSharedInformerFactory(f.kubeclient, noResyncPeriodFunc())

	f.podController = NewPodController(f.kubeInformer.Core().V1().Pods(), f.kubeclient, f.npMgr)
	f.podController.podListerSynced = alwaysReady

	for _, pod := range f.podLister {
		f.kubeInformer.Core().V1().Pods().Informer().GetIndexer().Add(pod)
	}
}

func (f *podFixture) ipSetSave() {
	//  call /sbin/ipset save -file /var/log/ipset-test.conf
	if err := f.ipsMgr.Save(util.IpsetTestConfigFile); err != nil {
		f.t.Errorf("TestAddPod failed @ ipsMgr.Save")
	}
}
func (f *podFixture) ipSetRestore() {
	//  call /sbin/ipset restore -file /var/log/ipset-test.conf
	if err := f.ipsMgr.Restore(util.IpsetTestConfigFile); err != nil {
		f.t.Errorf("TestAddPod failed @ ipsMgr.Restore")
	}
}

func newNPMgr(t *testing.T) *NetworkPolicyManager {
	npMgr := &NetworkPolicyManager{
		NsMap:            make(map[string]*Namespace),
		PodMap:           make(map[string]*NpmPod),
		RawNpMap:         make(map[string]*networkingv1.NetworkPolicy),
		ProcessedNpMap:   make(map[string]*networkingv1.NetworkPolicy),
		TelemetryEnabled: false,
	}

	// (TODO:) should remove error return
	// (Question) Is it necessary? Seems that it is necessary. Without this, it will have panic in PodController
	allNs, _ := newNs(util.KubeAllNamespacesFlag)
	npMgr.NsMap[util.KubeAllNamespacesFlag] = allNs
	return npMgr
}

// 1. clean up below function
func newPod(name, ns, rv string, labels map[string]string, status *corev1.PodStatus) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       ns,
			Labels:          labels,
			ResourceVersion: rv,
		},
		Status: *status,
	}
}
func newPodStatus(phase string, podIP string) *corev1.PodStatus {
	return &corev1.PodStatus{
		Phase: corev1.PodPhase(phase),
		PodIP: podIP,
	}
}

func createPod() *corev1.Pod {
	labels := map[string]string{
		"app": "test-pod",
	}
	status := newPodStatus("Running", "1.2.3.4")
	podObj := newPod("test-pod", "test-namespace", "0", labels, status)
	podObj.Spec = corev1.PodSpec{
		Containers: []corev1.Container{
			corev1.Container{
				Ports: []corev1.ContainerPort{
					corev1.ContainerPort{
						Name:          "app:test-pod",
						ContainerPort: 8080,
					},
				},
			},
		},
	}
	return podObj
}

func getKey(podObj *corev1.Pod, t *testing.T) string {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(podObj)
	if err != nil {
		t.Errorf("Unexpected error getting key for pod %s: %v", podObj.Name, err)
		return ""
	}
	return key
}

func addPod(t *testing.T, f *podFixture, podObj *corev1.Pod) {
	f.podLister = append(f.podLister, podObj)
	f.kubeobjects = append(f.kubeobjects, podObj)

	f.newPodController()
	stopCh := make(chan struct{})
	defer close(stopCh)
	f.kubeInformer.Start(stopCh)

	// simulate pod add event and add pod object to sharedInformer cache
	f.podController.addPod(podObj)
	if podObj.Spec.HostNetwork {
		return
	}

	err := f.podController.syncPod(getKey(podObj, t))
	if err != nil {
		f.t.Errorf("error syncing pod: %v", err)
	}

	// TODO: add more checking list and clean up with tables with expected values.
	if podObj.Spec.HostNetwork {
		if len(f.npMgr.PodMap) != 0 {
			t.Errorf("addPod failed @ PodMap length check -current len %d %v", len(f.npMgr.PodMap), f.npMgr.PodMap)
		}
	} else {
		if len(f.npMgr.PodMap) != 1 {
			t.Errorf("addPod failed @ PodMap length check - current len %d %v", len(f.npMgr.PodMap), f.npMgr.PodMap)
		}
	}

	if podObj.Spec.HostNetwork {
		if len(f.npMgr.NsMap) != 1 {
			t.Errorf("addPod failed @ nsMap length check - current len %d %v", len(f.npMgr.NsMap), f.npMgr.NsMap)
		}
	} else {
		if len(f.npMgr.NsMap) != 2 {
			//  all-namespaces and ns-test-namespace
			t.Errorf("addPod failed @ nsMap length check - current len %d %v", len(f.npMgr.NsMap), f.npMgr.NsMap)
		}
	}
}

// // TODO: who call this function for message - type?
func deletePod(t *testing.T, f *podFixture, podObj *corev1.Pod) {
	addPod(t, f, podObj)
	t.Logf("Complete add pod event")

	// simulate pod delete event and delete pod object from sharedInformer cache
	f.kubeInformer.Core().V1().Pods().Informer().GetIndexer().Delete(podObj)

	// call deletePod event
	f.podController.deletePod(podObj)
	if !podObj.Spec.HostNetwork {
		err := f.podController.syncPod(getKey(podObj, t))
		if err != nil {
			f.t.Errorf("error syncing pod: %v", err)
		}
	}

	// TODO: add more checking list and clean up with tables with expected values.
	if len(f.npMgr.PodMap) != 0 {
		t.Errorf("deletePod failed @ PodMap length check -current len %d %v", len(f.npMgr.PodMap), f.npMgr.PodMap)
	}

	if podObj.Spec.HostNetwork {
		if len(f.npMgr.NsMap) != 1 {
			t.Errorf("deletePod failed for HostNetwork @ nsMap length check - current len %d %v", len(f.npMgr.NsMap), f.npMgr.NsMap)
		}
	} else {
		if len(f.npMgr.NsMap) != 2 {
			//  all-namespaces and ns-test-namespace
			t.Errorf("deletePod failed @ nsMap length check - current len %d %v", len(f.npMgr.NsMap), f.npMgr.NsMap)
		}
	}
}

// Need to make more cases - interestings..
func updatePod(t *testing.T, f *podFixture, oldPodObj *corev1.Pod, newPodObj *corev1.Pod) {
	addPod(t, f, oldPodObj)
	t.Logf("Complete add pod event")

	// simulate pod update event and update the pod to shared informer's cache
	f.kubeInformer.Core().V1().Pods().Informer().GetIndexer().Update(newPodObj)
	f.podController.updatePod(oldPodObj, newPodObj)

	// call deletePod event
	if !newPodObj.Spec.HostNetwork {
		err := f.podController.syncPod(getKey(newPodObj, t))
		if err != nil {
			f.t.Errorf("error syncing pod: %v", err)
		}
	}

	// simulate pod delete event and delete pod object from sharedInformer cache
	f.kubeInformer.Core().V1().Pods().Informer().GetIndexer().Delete(oldPodObj)

	// call deletePod event
	f.podController.deletePod(oldPodObj)
	if !oldPodObj.Spec.HostNetwork {
		err := f.podController.syncPod(getKey(oldPodObj, t))
		if err != nil {
			f.t.Errorf("error syncing pod: %v", err)
		}
	}

	// TODO: add more checking list and clean up with tables with expected values.
	if newPodObj.Spec.HostNetwork {
		if len(f.npMgr.PodMap) != 0 {
			t.Errorf("updatePod failed @ PodMap length check -current len %d %v", len(f.npMgr.PodMap), f.npMgr.PodMap)
		}
	} else {
		if len(f.npMgr.PodMap) != 1 {
			t.Errorf("updatePod failed @ PodMap length check - current len %d %v", len(f.npMgr.PodMap), f.npMgr.PodMap)
		}
	}

	if newPodObj.Spec.HostNetwork {
		if len(f.npMgr.NsMap) != 1 {
			t.Errorf("updatePod failed @ nsMap length check - current len %d %v", len(f.npMgr.NsMap), f.npMgr.NsMap)
		}
	} else {
		if len(f.npMgr.NsMap) != 2 {
			//  all-namespaces and ns-test-namespace
			t.Errorf("updatePod failed @ nsMap length check - current len %d %v", len(f.npMgr.NsMap), f.npMgr.NsMap)
		}
	}

	podKey := getKey(newPodObj, t)
	cachedPodObj, exists := f.podController.npMgr.PodMap[podKey]
	if !exists {
		t.Errorf("TestOldRVUpdatePod failed @ pod exists check")
	}

	if cachedPodObj.ResourceVersion != 1 {
		t.Errorf("TestOldRVUpdatePod failed @ resourceVersion check - %d", cachedPodObj.ResourceVersion)
	}

	if !reflect.DeepEqual(cachedPodObj.Labels, newPodObj.Labels) {
		t.Errorf("TestOldRVUpdatePod failed @ labels check")
	}
}

func TestAddPod(t *testing.T) {
	f := newFixture(t)
	f.ipSetSave()
	defer f.ipSetRestore()

	podObj := createPod()
	podObj.Spec.HostNetwork = false
	addPod(t, f, podObj)
}

func TestAddHostNetworkPod(t *testing.T) {
	f := newFixture(t)
	f.ipSetSave()
	defer f.ipSetRestore()

	podObj := createPod()
	podObj.Spec.HostNetwork = true
	addPod(t, f, podObj)
}

func TestDeletePod(t *testing.T) {
	f := newFixture(t)
	f.ipSetSave()
	defer f.ipSetRestore()

	podObj := createPod()
	podObj.Spec.HostNetwork = false
	deletePod(t, f, podObj)
}

func TestDeleteHostNetworkPod(t *testing.T) {
	f := newFixture(t)
	f.ipSetSave()
	defer f.ipSetRestore()

	podObj := createPod()
	podObj.Spec.HostNetwork = true
	deletePod(t, f, podObj)
}

// need to clean up
func TestUpdatePod(t *testing.T) {
	f := newFixture(t)
	f.ipSetSave()
	defer f.ipSetRestore()

	oldRV := "0"
	newRV := "1"
	isHostNetwork := false

	labels := map[string]string{
		"app": "old-test-pod",
	}
	status := newPodStatus("Running", "1.2.3.4")
	oldPodObj := newPod("old-test-pod", "test-namespace", oldRV, labels, status)
	oldPodObj.Spec.HostNetwork = isHostNetwork

	labels = map[string]string{
		"app": "new-test-pod",
	}
	status = newPodStatus("Running", "4.3.2.1")
	newPodObj := newPod("new-test-pod", "test-namespace", newRV, labels, status)
	newPodObj.Spec.HostNetwork = isHostNetwork

	updatePod(t, f, oldPodObj, newPodObj)
}

func TestUpdateHostNetworkPod(t *testing.T) {
}

// // TODO: move this function to util file?
// func TestIsValidPod(t *testing.T) {
// 	podObj := &corev1.Pod{
// 		Status: corev1.PodStatus{
// 			Phase: "Running",
// 			PodIP: "1.2.3.4",
// 		},
// 	}
// 	if ok := isValidPod(podObj); !ok {
// 		t.Errorf("TestisValidPod failed @ isValidPod")
// 	}
// }

// // TODO: move this function to util file?
// func TestIsSystemPod(t *testing.T) {
// 	podObj := &corev1.Pod{
// 		ObjectMeta: metav1.ObjectMeta{
// 			Namespace: util.KubeSystemFlag,
// 		},
// 	}
// 	if ok := isSystemPod(podObj); !ok {
// 		t.Errorf("TestisSystemPod failed @ isSystemPod")
// 	}
// }
