// Copyright 2018 Microsoft. All rights reserved.
// MIT License
package npm

import (
	"fmt"
	"reflect"
	"strconv"
	"testing"

	"github.com/Azure/azure-container-networking/npm/ipsm"
	corev1 "k8s.io/api/core/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubeinformers "k8s.io/client-go/informers"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	core "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
)

const (
	HostNetwork    = true
	NonHostNetwork = false
)

type podFixture struct {
	t *testing.T

	kubeclient *k8sfake.Clientset
	// Objects to put in the store.
	podLister []*corev1.Pod
	// (TODO) Actions expected to happen on the client. Will use this to check action.
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

func (f *podFixture) newPodController(stopCh chan struct{}) {
	f.kubeclient = k8sfake.NewSimpleClientset(f.kubeobjects...)
	f.kubeInformer = kubeinformers.NewSharedInformerFactory(f.kubeclient, noResyncPeriodFunc())

	f.podController = NewPodController(f.kubeInformer.Core().V1().Pods(), f.kubeclient, f.npMgr)
	f.podController.podListerSynced = alwaysReady

	for _, pod := range f.podLister {
		f.kubeInformer.Core().V1().Pods().Informer().GetIndexer().Add(pod)
	}

	f.kubeInformer.Start(stopCh)
}

func (f *podFixture) ipSetSave(ipsetConfigFile string) {
	//  call /sbin/ipset save -file /var/log/ipset-test.conf
	f.t.Logf("Start storing ipset to %s", ipsetConfigFile)
	if err := f.ipsMgr.Save(ipsetConfigFile); err != nil {
		f.t.Errorf("TestAddPod failed @ ipsMgr.Save")
	}
}
func (f *podFixture) ipSetRestore(ipsetConfigFile string) {
	//  call /sbin/ipset restore -file /var/log/ipset-test.conf
	f.t.Logf("Start re-storing ipset to %s", ipsetConfigFile)
	if err := f.ipsMgr.Restore(ipsetConfigFile); err != nil {
		f.t.Errorf("TestAddPod failed @ ipsMgr.Restore")
	}
}

func createPod(name, ns, rv, podIP string, labels map[string]string, isHostNewtwork bool, podPhase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       ns,
			Labels:          labels,
			ResourceVersion: rv,
		},
		Spec: corev1.PodSpec{
			HostNetwork: isHostNewtwork,
			Containers: []corev1.Container{
				corev1.Container{
					Ports: []corev1.ContainerPort{
						corev1.ContainerPort{
							Name:          fmt.Sprintf("app:%s", name),
							ContainerPort: 8080,
							//Protocol:      "TCP",
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: podPhase,
			PodIP: podIP,
		},
	}
}

func addPod(t *testing.T, f *podFixture, podObj *corev1.Pod) {
	// simulate pod add event and add pod object to sharedInformer cache
	f.podController.addPod(podObj)

	if f.podController.workqueue.Len() == 0 {
		t.Logf("Add Pod: worker queue length is 0 ")
		return
	}

	f.podController.processNextWorkItem()
}

func deletePod(t *testing.T, f *podFixture, podObj *corev1.Pod, isDeletedFinalStateUnknownObject IsDeletedFinalStateUnknownObject) {
	addPod(t, f, podObj)
	t.Logf("Complete add pod event")

	// simulate pod delete event and delete pod object from sharedInformer cache
	f.kubeInformer.Core().V1().Pods().Informer().GetIndexer().Delete(podObj)

	if isDeletedFinalStateUnknownObject {
		podKey := getKey(podObj, t)
		tombstone := cache.DeletedFinalStateUnknown{
			Key: podKey,
			Obj: podObj,
		}
		f.podController.deletePod(tombstone)
	} else {
		f.podController.deletePod(podObj)
	}

	if f.podController.workqueue.Len() == 0 {
		t.Logf("Delete Pod: worker queue length is 0 ")
		return
	}

	f.podController.processNextWorkItem()
}

// Need to make more cases - interestings..
func updatePod(t *testing.T, f *podFixture, oldPodObj *corev1.Pod, newPodObj *corev1.Pod) {
	addPod(t, f, oldPodObj)
	t.Logf("Complete add pod event")

	// simulate pod update event and update the pod to shared informer's cache
	f.kubeInformer.Core().V1().Pods().Informer().GetIndexer().Update(newPodObj)
	f.podController.updatePod(oldPodObj, newPodObj)

	if f.podController.workqueue.Len() == 0 {
		t.Logf("Update Pod: worker queue length is 0 ")
		return
	}

	f.podController.processNextWorkItem()
}

type expectedValues struct {
	expectedLenOfPodMap    int
	expectedLenOfNsMap     int
	expectedLenOfWorkQueue int
}

func checkPodTestResult(testName string, f *podFixture, testCases []expectedValues) {
	for _, test := range testCases {
		if got := len(f.podController.PodMap); got != test.expectedLenOfPodMap {
			f.t.Errorf("%s failed @ PodMap length = %d, want %d", testName, got, test.expectedLenOfPodMap)
		}
		if got := len(f.npMgr.NsMap); got != test.expectedLenOfNsMap {
			f.t.Errorf("%s failed @ NsMap length = %d, want %d", testName, got, test.expectedLenOfNsMap)
		}
		if got := f.podController.workqueue.Len(); got != test.expectedLenOfWorkQueue {
			f.t.Errorf("%s failed @ Workqueue length = %d, want %d", testName, got, test.expectedLenOfWorkQueue)
		}
	}
}

func checkNpmPodWithInput(testName string, f *podFixture, inputPodObj *corev1.Pod) {
	podKey := getKey(inputPodObj, f.t)
	cachedNpmPodObj := f.podController.PodMap[podKey]

	if cachedNpmPodObj.PodIP != inputPodObj.Status.PodIP {
		f.t.Errorf("%s failed @ PodIp check got = %s, want %s", testName, cachedNpmPodObj.PodIP, inputPodObj.Status.PodIP)
	}

	if !reflect.DeepEqual(cachedNpmPodObj.Labels, inputPodObj.Labels) {
		f.t.Errorf("%s failed @ Labels check got = %v, want %v", testName, cachedNpmPodObj.Labels, inputPodObj.Labels)
	}

	inputPortList := getContainerPortList(inputPodObj)
	if !reflect.DeepEqual(cachedNpmPodObj.ContainerPorts, inputPortList) {
		f.t.Errorf("%s failed @ Container port check got = %v, want %v", testName, cachedNpmPodObj.PodIP, inputPortList)
	}
}

func TestAddMultiplePods(t *testing.T) {
	labels := map[string]string{
		"app": "test-pod",
	}
	podObj1 := createPod("test-pod-1", "test-namespace", "0", "1.2.3.4", labels, NonHostNetwork, corev1.PodRunning)
	podObj2 := createPod("test-pod-2", "test-namespace", "0", "1.2.3.5", labels, NonHostNetwork, corev1.PodRunning)

	f := newFixture(t)
	f.podLister = append(f.podLister, podObj1, podObj2)
	f.kubeobjects = append(f.kubeobjects, podObj1, podObj2)
	stopCh := make(chan struct{})
	defer close(stopCh)
	f.newPodController(stopCh)

	addPod(t, f, podObj1)
	addPod(t, f, podObj2)

	testCases := []expectedValues{
		{2, 2, 0},
	}
	checkPodTestResult("TestAddMultiplePods", f, testCases)
	checkNpmPodWithInput("TestAddMultiplePods", f, podObj1)
	checkNpmPodWithInput("TestAddMultiplePods", f, podObj2)
}

func TestAddPod(t *testing.T) {
	labels := map[string]string{
		"app": "test-pod",
	}
	podObj := createPod("test-pod", "test-namespace", "0", "1.2.3.4", labels, NonHostNetwork, corev1.PodRunning)

	f := newFixture(t)
	f.podLister = append(f.podLister, podObj)
	f.kubeobjects = append(f.kubeobjects, podObj)
	stopCh := make(chan struct{})
	defer close(stopCh)
	f.newPodController(stopCh)

	addPod(t, f, podObj)
	testCases := []expectedValues{
		{1, 2, 0},
	}
	checkPodTestResult("TestAddPod", f, testCases)
	checkNpmPodWithInput("TestAddPod", f, podObj)
}

func TestAddHostNetworkPod(t *testing.T) {
	labels := map[string]string{
		"app": "test-pod",
	}
	podObj := createPod("test-pod", "test-namespace", "0", "1.2.3.4", labels, HostNetwork, corev1.PodRunning)
	podKey := getKey(podObj, t)

	f := newFixture(t)
	f.podLister = append(f.podLister, podObj)
	f.kubeobjects = append(f.kubeobjects, podObj)
	stopCh := make(chan struct{})
	defer close(stopCh)
	f.newPodController(stopCh)

	addPod(t, f, podObj)
	testCases := []expectedValues{
		{0, 1, 0},
	}
	checkPodTestResult("TestAddHostNetworkPod", f, testCases)

	if _, exists := f.podController.PodMap[podKey]; exists {
		t.Error("TestAddHostNetworkPod failed @ cached pod obj exists check")
	}
}

func TestDeletePod(t *testing.T) {
	labels := map[string]string{
		"app": "test-pod",
	}
	podObj := createPod("test-pod", "test-namespace", "0", "1.2.3.4", labels, NonHostNetwork, corev1.PodRunning)
	podKey := getKey(podObj, t)

	f := newFixture(t)
	f.podLister = append(f.podLister, podObj)
	f.kubeobjects = append(f.kubeobjects, podObj)
	stopCh := make(chan struct{})
	defer close(stopCh)
	f.newPodController(stopCh)

	deletePod(t, f, podObj, DeletedFinalStateknownObject)
	testCases := []expectedValues{
		{0, 2, 0},
	}
	checkPodTestResult("TestDeletePod", f, testCases)
	if _, exists := f.podController.PodMap[podKey]; exists {
		t.Error("TestDeletePod failed @ cached pod obj exists check")
	}
}

func TestDeleteHostNetworkPod(t *testing.T) {
	labels := map[string]string{
		"app": "test-pod",
	}
	podObj := createPod("test-pod", "test-namespace", "0", "1.2.3.4", labels, HostNetwork, corev1.PodRunning)
	podKey := getKey(podObj, t)

	f := newFixture(t)
	f.podLister = append(f.podLister, podObj)
	f.kubeobjects = append(f.kubeobjects, podObj)
	stopCh := make(chan struct{})
	defer close(stopCh)
	f.newPodController(stopCh)

	deletePod(t, f, podObj, DeletedFinalStateknownObject)
	testCases := []expectedValues{
		{0, 1, 0},
	}
	checkPodTestResult("TestDeleteHostNetworkPod", f, testCases)
	if _, exists := f.podController.PodMap[podKey]; exists {
		t.Error("TestDeleteHostNetworkPod failed @ cached pod obj exists check")
	}
}

func TestDeletePodWithTombstone(t *testing.T) {
	labels := map[string]string{
		"app": "test-pod",
	}
	podObj := createPod("test-pod", "test-namespace", "0", "1.2.3.4", labels, NonHostNetwork, corev1.PodRunning)
	f := newFixture(t)
	stopCh := make(chan struct{})
	defer close(stopCh)
	f.newPodController(stopCh)

	podKey := getKey(podObj, t)
	tombstone := cache.DeletedFinalStateUnknown{
		Key: podKey,
		Obj: podObj,
	}

	f.podController.deletePod(tombstone)
	testCases := []expectedValues{
		{0, 1, 0},
	}
	checkPodTestResult("TestDeletePodWithTombstone", f, testCases)
}

func TestDeletePodWithTombstoneAfterAddingPod(t *testing.T) {
	labels := map[string]string{
		"app": "test-pod",
	}
	podObj := createPod("test-pod", "test-namespace", "0", "1.2.3.4", labels, NonHostNetwork, corev1.PodRunning)

	f := newFixture(t)
	f.podLister = append(f.podLister, podObj)
	f.kubeobjects = append(f.kubeobjects, podObj)
	stopCh := make(chan struct{})
	defer close(stopCh)
	f.newPodController(stopCh)

	deletePod(t, f, podObj, DeletedFinalStateUnknownObject)
	testCases := []expectedValues{
		{0, 2, 0},
	}
	checkPodTestResult("TestDeletePodWithTombstoneAfterAddingPod", f, testCases)
}

func TestLabelUpdatePod(t *testing.T) {
	labels := map[string]string{
		"app": "test-pod",
	}
	oldPodObj := createPod("test-pod", "test-namespace", "0", "1.2.3.4", labels, NonHostNetwork, corev1.PodRunning)

	f := newFixture(t)
	f.podLister = append(f.podLister, oldPodObj)
	f.kubeobjects = append(f.kubeobjects, oldPodObj)
	stopCh := make(chan struct{})
	defer close(stopCh)
	f.newPodController(stopCh)

	newPodObj := oldPodObj.DeepCopy()
	newPodObj.Labels = map[string]string{
		"app": "new-test-pod",
	}
	// oldPodObj.ResourceVersion value is "0"
	newRV, _ := strconv.Atoi(oldPodObj.ResourceVersion)
	newPodObj.ResourceVersion = fmt.Sprintf("%d", newRV+1)
	updatePod(t, f, oldPodObj, newPodObj)

	testCases := []expectedValues{
		{1, 2, 0},
	}
	checkPodTestResult("TestLabelUpdatePod", f, testCases)
	checkNpmPodWithInput("TestLabelUpdatePod", f, newPodObj)
}

func TestIPAddressUpdatePod(t *testing.T) {
	labels := map[string]string{
		"app": "test-pod",
	}
	oldPodObj := createPod("test-pod", "test-namespace", "0", "1.2.3.4", labels, NonHostNetwork, corev1.PodRunning)

	f := newFixture(t)
	f.podLister = append(f.podLister, oldPodObj)
	f.kubeobjects = append(f.kubeobjects, oldPodObj)
	stopCh := make(chan struct{})
	defer close(stopCh)
	f.newPodController(stopCh)

	newPodObj := oldPodObj.DeepCopy()
	// oldPodObj.ResourceVersion value is "0"
	newRV, _ := strconv.Atoi(oldPodObj.ResourceVersion)
	newPodObj.ResourceVersion = fmt.Sprintf("%d", newRV+1)
	// oldPodObj PodIP is "1.2.3.4"
	newPodObj.Status.PodIP = "4.3.2.1"
	updatePod(t, f, oldPodObj, newPodObj)

	testCases := []expectedValues{
		{1, 2, 0},
	}
	checkPodTestResult("TestIPAddressUpdatePod", f, testCases)
	checkNpmPodWithInput("TestIPAddressUpdatePod", f, newPodObj)
}

func TestPodStatusUpdatePod(t *testing.T) {
	labels := map[string]string{
		"app": "test-pod",
	}
	oldPodObj := createPod("test-pod", "test-namespace", "0", "1.2.3.4", labels, NonHostNetwork, corev1.PodRunning)
	podKey := getKey(oldPodObj, t)

	f := newFixture(t)
	f.podLister = append(f.podLister, oldPodObj)
	f.kubeobjects = append(f.kubeobjects, oldPodObj)
	stopCh := make(chan struct{})
	defer close(stopCh)
	f.newPodController(stopCh)

	newPodObj := oldPodObj.DeepCopy()
	// oldPodObj.ResourceVersion value is "0"
	newRV, _ := strconv.Atoi(oldPodObj.ResourceVersion)
	newPodObj.ResourceVersion = fmt.Sprintf("%d", newRV+1)

	// oldPodObj PodIP is "1.2.3.4"
	newPodObj.Status.Phase = corev1.PodSucceeded
	updatePod(t, f, oldPodObj, newPodObj)

	testCases := []expectedValues{
		{0, 2, 0},
	}
	checkPodTestResult("TestPodStatusUpdatePod", f, testCases)
	if _, exists := f.podController.PodMap[podKey]; exists {
		t.Error("TestPodStatusUpdatePod failed @ cached pod obj exists check")
	}
}

func TestHasValidPodIP(t *testing.T) {
	podObj := &corev1.Pod{
		Status: corev1.PodStatus{
			Phase: "Running",
			PodIP: "1.2.3.4",
		},
	}
	if ok := hasValidPodIP(podObj); !ok {
		t.Errorf("TestisValidPod failed @ isValidPod")
	}
}
