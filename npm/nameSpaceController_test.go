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

type expectedNsValues struct {
	expectedLenOfPodMap    int
	expectedLenOfNsMap     int
	expectedLenOfWorkQueue int
}

type nameSpaceFixture struct {
	t *testing.T

	kubeclient *k8sfake.Clientset
	// Objects to put in the store.
	nsLister []*corev1.Namespace
	// Actions expected to happen on the client.
	kubeactions []core.Action
	// Objects from here preloaded into NewSimpleFake.
	kubeobjects []runtime.Object

	ipsMgr       *ipsm.IpsetManager
	nsController *nameSpaceController
	kubeInformer kubeinformers.SharedInformerFactory
}

func newNsFixture(t *testing.T) *nameSpaceFixture {
	f := &nameSpaceFixture{
		t:           t,
		nsLister:    []*corev1.Namespace{},
		kubeobjects: []runtime.Object{},
		ipsMgr:      ipsm.NewIpsetManager(),
	}
	return f
}

func (f *nameSpaceFixture) newNsController(stopCh chan struct{}) {
	f.kubeclient = k8sfake.NewSimpleClientset(f.kubeobjects...)
	f.kubeInformer = kubeinformers.NewSharedInformerFactory(f.kubeclient, noResyncPeriodFunc())

	npmNamespaceCache := &npmNamespaceCache{nsMap: make(map[string]*Namespace)}
	f.nsController = NewNameSpaceController(f.kubeInformer.Core().V1().Namespaces(), f.kubeclient, f.ipsMgr, npmNamespaceCache)
	f.nsController.nameSpaceListerSynced = alwaysReady

	for _, ns := range f.nsLister {
		f.kubeInformer.Core().V1().Namespaces().Informer().GetIndexer().Add(ns)
	}

	f.kubeInformer.Start(stopCh)
}

func (f *nameSpaceFixture) ipSetSave(ipsetConfigFile string) {
	//  call /sbin/ipset save -file /var/log/ipset-test.conf
	f.t.Logf("Start storing ipset to %s", ipsetConfigFile)
	if err := f.ipsMgr.Save(ipsetConfigFile); err != nil {
		f.t.Errorf("TestAddPod failed @ ipsMgr.Save")
	}
}

func (f *nameSpaceFixture) ipSetRestore(ipsetConfigFile string) {
	//  call /sbin/ipset restore -file /var/log/ipset-test.conf
	f.t.Logf("Start re-storing ipset to %s", ipsetConfigFile)
	if err := f.ipsMgr.Restore(ipsetConfigFile); err != nil {
		f.t.Errorf("TestAddPod failed @ ipsMgr.Restore")
	}
}

func newNameSpace(name, rv string, labels map[string]string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Labels:          labels,
			ResourceVersion: rv,
		},
	}
}

func addNamespace(t *testing.T, f *nameSpaceFixture, nsObj *corev1.Namespace) {
	t.Logf("Calling add namespace event")
	f.nsController.addNamespace(nsObj)
	if f.nsController.workqueue.Len() == 0 {
		t.Logf("Add Namespace: worker queue length is 0 ")
		return
	}
	f.nsController.processNextWorkItem()
}

func updateNamespace(t *testing.T, f *nameSpaceFixture, oldNsObj, newNsObj *corev1.Namespace) {
	addNamespace(t, f, oldNsObj)
	t.Logf("Complete add namespace event")

	t.Logf("Updating kubeinformer namespace object")
	f.kubeInformer.Core().V1().Namespaces().Informer().GetIndexer().Update(newNsObj)

	t.Logf("Calling update namespace event")
	f.nsController.updateNamespace(oldNsObj, newNsObj)
	if f.nsController.workqueue.Len() == 0 {
		t.Logf("Update Namespace: worker queue length is 0 ")
		return
	}
	f.nsController.processNextWorkItem()
}

func deleteNamespace(t *testing.T, f *nameSpaceFixture, nsObj *corev1.Namespace, isDeletedFinalStateUnknownObject IsDeletedFinalStateUnknownObject) {
	addNamespace(t, f, nsObj)
	t.Logf("Complete add namespace event")

	t.Logf("Updating kubeinformer namespace object")
	f.kubeInformer.Core().V1().Namespaces().Informer().GetIndexer().Delete(nsObj)

	t.Logf("Calling delete namespace event")
	if isDeletedFinalStateUnknownObject {
		tombstone := cache.DeletedFinalStateUnknown{
			Key: nsObj.Name,
			Obj: nsObj,
		}
		f.nsController.deleteNamespace(tombstone)
	} else {
		f.nsController.deleteNamespace(nsObj)
	}

	if f.nsController.workqueue.Len() == 0 {
		t.Logf("Delete Namespace: worker queue length is 0 ")
		return
	}
	f.nsController.processNextWorkItem()
}

func TestAddNamespace(t *testing.T) {
	f := newNsFixture(t)
	f.ipSetSave(util.IpsetTestConfigFile)
	defer f.ipSetRestore(util.IpsetTestConfigFile)

	nsObj := newNameSpace(
		"test-namespace",
		"0",
		map[string]string{
			"app": "test-namespace",
		},
	)
	f.nsLister = append(f.nsLister, nsObj)
	f.kubeobjects = append(f.kubeobjects, nsObj)

	stopCh := make(chan struct{})
	defer close(stopCh)
	f.newNsController(stopCh)

	addNamespace(t, f, nsObj)

	testCases := []expectedNsValues{
		{0, 2, 0},
	}
	checkNsTestResult("TestAddNamespace", f, testCases)

	if _, exists := f.nsController.npmNamespaceCache.nsMap[util.GetNSNameWithPrefix(nsObj.Name)]; !exists {
		t.Errorf("TestAddNamespace failed @ npMgr.nsMap check")
	}
}

func TestUpdateNamespace(t *testing.T) {
	f := newNsFixture(t)
	f.ipSetSave(util.IpsetTestConfigFile)
	defer f.ipSetRestore(util.IpsetTestConfigFile)

	oldNsObj := newNameSpace(
		"test-namespace",
		"0",
		map[string]string{
			"app": "test-namespace",
		},
	)

	newNsObj := newNameSpace(
		"test-namespace",
		"1",
		map[string]string{
			"app": "new-test-namespace",
		},
	)
	f.nsLister = append(f.nsLister, oldNsObj)
	f.kubeobjects = append(f.kubeobjects, oldNsObj)

	stopCh := make(chan struct{})
	defer close(stopCh)
	f.newNsController(stopCh)
	updateNamespace(t, f, oldNsObj, newNsObj)

	testCases := []expectedNsValues{
		{0, 2, 0},
	}
	checkNsTestResult("TestUpdateNamespace", f, testCases)

	if _, exists := f.nsController.npmNamespaceCache.nsMap[util.GetNSNameWithPrefix(newNsObj.Name)]; !exists {
		t.Errorf("TestUpdateNamespace failed @ npMgr.nsMap check")
	}

	if !reflect.DeepEqual(
		newNsObj.Labels,
		f.nsController.npmNamespaceCache.nsMap[util.GetNSNameWithPrefix(oldNsObj.Name)].LabelsMap,
	) {
		t.Fatalf("TestUpdateNamespace failed @ npMgr.nsMap labelMap check")
	}
}

func TestAddNamespaceLabel(t *testing.T) {
	f := newNsFixture(t)
	f.ipSetSave(util.IpsetTestConfigFile)
	defer f.ipSetRestore(util.IpsetTestConfigFile)

	oldNsObj := newNameSpace(
		"test-namespace",
		"0",
		map[string]string{
			"app": "test-namespace",
		},
	)
	newNsObj := newNameSpace(
		"test-namespace",
		"1",
		map[string]string{
			"app":    "new-test-namespace",
			"update": "true",
		},
	)
	f.nsLister = append(f.nsLister, oldNsObj)
	f.kubeobjects = append(f.kubeobjects, oldNsObj)

	stopCh := make(chan struct{})
	defer close(stopCh)
	f.newNsController(stopCh)
	updateNamespace(t, f, oldNsObj, newNsObj)

	testCases := []expectedNsValues{
		{0, 2, 0},
	}
	checkNsTestResult("TestAddNamespaceLabel", f, testCases)

	if _, exists := f.nsController.npmNamespaceCache.nsMap[util.GetNSNameWithPrefix(newNsObj.Name)]; !exists {
		t.Errorf("TestAddNamespaceLabel failed @ nsMap check")
	}

	if !reflect.DeepEqual(
		newNsObj.Labels,
		f.nsController.npmNamespaceCache.nsMap[util.GetNSNameWithPrefix(oldNsObj.Name)].LabelsMap,
	) {
		t.Fatalf("TestAddNamespaceLabel failed @ nsMap labelMap check")
	}
}

func TestAddNamespaceLabelSameRv(t *testing.T) {
	f := newNsFixture(t)
	f.ipSetSave(util.IpsetTestConfigFile)
	defer f.ipSetRestore(util.IpsetTestConfigFile)

	oldNsObj := newNameSpace(
		"test-namespace",
		"0",
		map[string]string{
			"app": "test-namespace",
		},
	)

	newNsObj := newNameSpace(
		"test-namespace",
		"0",
		map[string]string{
			"app":    "new-test-namespace",
			"update": "true",
		},
	)
	f.nsLister = append(f.nsLister, oldNsObj)
	f.kubeobjects = append(f.kubeobjects, oldNsObj)

	stopCh := make(chan struct{})
	defer close(stopCh)
	f.newNsController(stopCh)
	updateNamespace(t, f, oldNsObj, newNsObj)

	testCases := []expectedNsValues{
		{0, 2, 0},
	}
	checkNsTestResult("TestAddNamespaceLabelSameRv", f, testCases)

	if _, exists := f.nsController.npmNamespaceCache.nsMap[util.GetNSNameWithPrefix(newNsObj.Name)]; !exists {
		t.Errorf("TestAddNamespaceLabelSameRv failed @ nsMap check")
	}

	if !reflect.DeepEqual(
		oldNsObj.Labels,
		f.nsController.npmNamespaceCache.nsMap[util.GetNSNameWithPrefix(oldNsObj.Name)].LabelsMap,
	) {
		t.Fatalf("TestAddNamespaceLabelSameRv failed @ nsMap labelMap check")
	}
}

func TestDeleteandUpdateNamespaceLabel(t *testing.T) {
	f := newNsFixture(t)
	f.ipSetSave(util.IpsetTestConfigFile)
	defer f.ipSetRestore(util.IpsetTestConfigFile)

	oldNsObj := newNameSpace(
		"test-namespace",
		"0",
		map[string]string{
			"app":    "old-test-namespace",
			"update": "true",
			"group":  "test",
		},
	)

	newNsObj := newNameSpace(
		"test-namespace",
		"1",
		map[string]string{
			"app":    "old-test-namespace",
			"update": "false",
		},
	)
	f.nsLister = append(f.nsLister, oldNsObj)
	f.kubeobjects = append(f.kubeobjects, oldNsObj)

	stopCh := make(chan struct{})
	defer close(stopCh)
	f.newNsController(stopCh)
	updateNamespace(t, f, oldNsObj, newNsObj)

	testCases := []expectedNsValues{
		{0, 2, 0},
	}
	checkNsTestResult("TestDeleteandUpdateNamespaceLabel", f, testCases)

	if _, exists := f.nsController.npmNamespaceCache.nsMap[util.GetNSNameWithPrefix(newNsObj.Name)]; !exists {
		t.Errorf("TestDeleteandUpdateNamespaceLabel failed @ nsMap check")
	}

	if !reflect.DeepEqual(
		newNsObj.Labels,
		f.nsController.npmNamespaceCache.nsMap[util.GetNSNameWithPrefix(oldNsObj.Name)].LabelsMap,
	) {
		t.Fatalf("TestDeleteandUpdateNamespaceLabel failed @ nsMap labelMap check")
	}
}

// TestNewNameSpaceUpdate will test the case where the key is same but the object is different.
// this happens when NSA delete event is missed and deleted from NPMLocalCache,
// but NSA gets added again. This will result in an update event with old and new with different UUIDs
func TestNewNameSpaceUpdate(t *testing.T) {
	f := newNsFixture(t)
	f.ipSetSave(util.IpsetTestConfigFile)
	defer f.ipSetRestore(util.IpsetTestConfigFile)

	oldNsObj := newNameSpace(
		"test-namespace",
		"10",
		map[string]string{
			"app":    "old-test-namespace",
			"update": "true",
			"group":  "test",
		},
	)
	oldNsObj.SetUID("test1")

	newNsObj := newNameSpace(
		"test-namespace",
		"9",
		map[string]string{
			"app":    "old-test-namespace",
			"update": "false",
		},
	)
	f.nsLister = append(f.nsLister, oldNsObj)
	f.kubeobjects = append(f.kubeobjects, oldNsObj)

	stopCh := make(chan struct{})
	defer close(stopCh)
	f.newNsController(stopCh)
	newNsObj.SetUID("test2")
	updateNamespace(t, f, oldNsObj, newNsObj)

	testCases := []expectedNsValues{
		{0, 2, 0},
	}
	checkNsTestResult("TestDeleteandUpdateNamespaceLabel", f, testCases)

	if _, exists := f.nsController.npmNamespaceCache.nsMap[util.GetNSNameWithPrefix(newNsObj.Name)]; !exists {
		t.Errorf("TestDeleteandUpdateNamespaceLabel failed @ nsMap check")
	}

	if !reflect.DeepEqual(
		newNsObj.Labels,
		f.nsController.npmNamespaceCache.nsMap[util.GetNSNameWithPrefix(oldNsObj.Name)].LabelsMap,
	) {
		t.Fatalf("TestDeleteandUpdateNamespaceLabel failed @ nsMap labelMap check")
	}
}

func TestDeleteNamespace(t *testing.T) {
	f := newNsFixture(t)
	f.ipSetSave(util.IpsetTestConfigFile)
	defer f.ipSetRestore(util.IpsetTestConfigFile)

	nsObj := newNameSpace(
		"test-namespace",
		"0",
		map[string]string{
			"app": "test-namespace",
		},
	)
	f.nsLister = append(f.nsLister, nsObj)
	f.kubeobjects = append(f.kubeobjects, nsObj)

	stopCh := make(chan struct{})
	defer close(stopCh)
	f.newNsController(stopCh)
	deleteNamespace(t, f, nsObj, DeletedFinalStateknownObject)

	testCases := []expectedNsValues{
		{0, 1, 0},
	}
	checkNsTestResult("TestDeleteNamespace", f, testCases)

	if _, exists := f.nsController.npmNamespaceCache.nsMap[util.GetNSNameWithPrefix(nsObj.Name)]; exists {
		t.Errorf("TestDeleteNamespace failed @ nsMap check")
	}
}

func TestDeleteNamespaceWithTombstone(t *testing.T) {
	f := newNsFixture(t)
	f.ipSetSave(util.IpsetTestConfigFile)
	defer f.ipSetRestore(util.IpsetTestConfigFile)
	stopCh := make(chan struct{})
	defer close(stopCh)
	f.newNsController(stopCh)

	nsObj := newNameSpace(
		"test-namespace",
		"0",
		map[string]string{
			"app": "test-namespace",
		},
	)
	tombstone := cache.DeletedFinalStateUnknown{
		Key: nsObj.Name,
		Obj: nsObj,
	}

	f.nsController.deleteNamespace(tombstone)

	testCases := []expectedNsValues{
		{0, 1, 0},
	}
	checkNsTestResult("TestDeleteNamespaceWithTombstone", f, testCases)
}

func TestDeleteNamespaceWithTombstoneAfterAddingNameSpace(t *testing.T) {
	nsObj := newNameSpace(
		"test-namespace",
		"0",
		map[string]string{
			"app": "test-namespace",
		},
	)

	f := newNsFixture(t)
	f.nsLister = append(f.nsLister, nsObj)
	f.kubeobjects = append(f.kubeobjects, nsObj)
	stopCh := make(chan struct{})
	defer close(stopCh)
	f.newNsController(stopCh)

	deleteNamespace(t, f, nsObj, DeletedFinalStateUnknownObject)
	testCases := []expectedNsValues{
		{0, 1, 0},
	}
	checkNsTestResult("TestDeleteNamespaceWithTombstoneAfterAddingNameSpace", f, testCases)
}

func TestGetNamespaceObjFromNsObj(t *testing.T) {
	ns := newNs("test-ns")
	ns.LabelsMap = map[string]string{
		"test": "new",
	}

	nsObj := ns.getNamespaceObjFromNsObj()

	if !reflect.DeepEqual(ns.LabelsMap, nsObj.ObjectMeta.Labels) {
		t.Errorf("TestGetNamespaceObjFromNsObj failed @ nsObj labels check")
	}
}

func TestIsSystemNs(t *testing.T) {
	nsObj := newNameSpace("kube-system", "0", map[string]string{"test": "new"})

	if !isSystemNs(nsObj) {
		t.Errorf("TestIsSystemNs failed @ nsObj isSystemNs check")
	}
}

func checkNsTestResult(testName string, f *nameSpaceFixture, testCases []expectedNsValues) {
	for _, test := range testCases {
		if got := len(f.nsController.npmNamespaceCache.nsMap); got != test.expectedLenOfNsMap {
			f.t.Errorf("NsMap length = %d, want %d. Map: %+v", got, test.expectedLenOfNsMap, f.nsController.npmNamespaceCache.nsMap)
		}
		if got := f.nsController.workqueue.Len(); got != test.expectedLenOfWorkQueue {
			f.t.Errorf("Workqueue length = %d, want %d", got, test.expectedLenOfWorkQueue)
		}
	}
}
