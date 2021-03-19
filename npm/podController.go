// Copyright 2018 Microsoft. All rights reserved.
// MIT License
package npm

import (
	"fmt"
	"reflect"
	"time"

	"github.com/Azure/azure-container-networking/log"
	"github.com/Azure/azure-container-networking/npm/ipsm"
	"github.com/Azure/azure-container-networking/npm/metrics"
	"github.com/Azure/azure-container-networking/npm/util"

	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	coreinformer "k8s.io/client-go/informers/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

type NpmPod struct {
	Name            string
	Namespace       string
	NodeName        string
	PodUID          string
	PodIP           string
	IsHostNetwork   bool
	PodIPs          []v1.PodIP
	Labels          map[string]string
	ContainerPorts  []v1.ContainerPort
	ResourceVersion uint64 // Pod Resource Version
	Phase           corev1.PodPhase
}

func newNpmPod(podObj *corev1.Pod) (*NpmPod, error) {
	rv := util.ParseResourceVersion(podObj.GetObjectMeta().GetResourceVersion())
	pod := &NpmPod{
		Name:            podObj.ObjectMeta.Name,
		Namespace:       podObj.ObjectMeta.Namespace,
		NodeName:        podObj.Spec.NodeName,
		PodUID:          string(podObj.ObjectMeta.UID),
		PodIP:           podObj.Status.PodIP,
		PodIPs:          podObj.Status.PodIPs,
		IsHostNetwork:   podObj.Spec.HostNetwork,
		Labels:          podObj.Labels,
		ContainerPorts:  getContainerPortList(podObj),
		ResourceVersion: rv,
		Phase:           podObj.Status.Phase,
	}

	return pod, nil
}

func getPodObjFromNpmObj(npmPodObj *NpmPod) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      npmPodObj.Name,
			Namespace: npmPodObj.Namespace,
			Labels:    npmPodObj.Labels,
			UID:       types.UID(npmPodObj.PodUID),
		},
		Status: corev1.PodStatus{
			Phase:  npmPodObj.Phase,
			PodIP:  npmPodObj.PodIP,
			PodIPs: npmPodObj.PodIPs,
		},
		Spec: corev1.PodSpec{
			HostNetwork: npmPodObj.IsHostNetwork,
			NodeName:    npmPodObj.NodeName,
			Containers: []v1.Container{
				v1.Container{
					Ports: npmPodObj.ContainerPorts,
				},
			},
		},
	}

}

func isValidPod(podObj *corev1.Pod) bool {
	return len(podObj.Status.PodIP) > 0
}

func isSystemPod(podObj *corev1.Pod) bool {
	return podObj.ObjectMeta.Namespace == util.KubeSystemFlag
}

func isHostNetworkPod(podObj *corev1.Pod) bool {
	return podObj.Spec.HostNetwork
}

func isInvalidPodUpdate(oldPodObj, newPodObj *corev1.Pod) (isInvalidUpdate bool) {
	isInvalidUpdate = oldPodObj.ObjectMeta.Namespace == newPodObj.ObjectMeta.Namespace &&
		oldPodObj.ObjectMeta.Name == newPodObj.ObjectMeta.Name &&
		oldPodObj.Status.Phase == newPodObj.Status.Phase &&
		oldPodObj.Status.PodIP == newPodObj.Status.PodIP &&
		newPodObj.ObjectMeta.DeletionTimestamp == nil &&
		newPodObj.ObjectMeta.DeletionGracePeriodSeconds == nil
	isInvalidUpdate = isInvalidUpdate &&
		reflect.DeepEqual(oldPodObj.ObjectMeta.Labels, newPodObj.ObjectMeta.Labels) &&
		reflect.DeepEqual(oldPodObj.Status.PodIPs, newPodObj.Status.PodIPs) &&
		reflect.DeepEqual(getContainerPortList(oldPodObj), getContainerPortList(newPodObj))

	return
}

func getContainerPortList(podObj *corev1.Pod) []v1.ContainerPort {
	portList := []v1.ContainerPort{}
	for _, container := range podObj.Spec.Containers {
		portList = append(portList, container.Ports...)
	}
	return portList
}

// appendNamedPortIpsets helps with adding or deleting Pod namedPort IPsets
func appendNamedPortIpsets(ipsMgr *ipsm.IpsetManager, portList []v1.ContainerPort, podUID string, podIP string, delete bool) error {

	for _, port := range portList {
		if port.Name == "" {
			continue
		}

		protocol := ""

		switch port.Protocol {
		case v1.ProtocolUDP:
			protocol = util.IpsetUDPFlag
		case v1.ProtocolSCTP:
			protocol = util.IpsetSCTPFlag
		case v1.ProtocolTCP:
			protocol = util.IpsetTCPFlag
		}

		namedPortname := util.NamedPortIPSetPrefix + port.Name

		if delete {
			// Delete the pod's named ports from its ipset.
			ipsMgr.DeleteFromSet(
				namedPortname,
				fmt.Sprintf("%s,%s%d", podIP, protocol, port.ContainerPort),
				podUID,
			)
			continue
		}
		// Add the pod's named ports to its ipset.
		ipsMgr.AddToSet(
			namedPortname,
			fmt.Sprintf("%s,%s%d", podIP, protocol, port.ContainerPort),
			util.IpsetIPPortHashFlag,
			podUID,
		)

	}

	return nil
}

// GetPodKey will return podKey
func GetPodKey(podObj *corev1.Pod) string {
	podKey, err := util.GetObjKeyFunc(podObj)
	if err != nil {
		metrics.SendErrorLogAndMetric(util.PodID, "[GetPodKey] Error: while running MetaNamespaceKeyFunc err: %s", err)
		return ""
	}
	podKey = podKey + "/" + string(podObj.GetObjectMeta().GetUID())
	return util.GetNSNameWithPrefix(podKey)
}

type podController struct {
	clientset       *kubernetes.Clientset
	podLister       corelisters.PodLister
	podListerSynced cache.InformerSynced
	workqueue       workqueue.RateLimitingInterface
	podCache        map[string]*v1.Pod

	// TODO: change name to npmSharedCache
	// c.npMgr.NsMap[podNs] and c.npMgr.PodMap[podKey], if there are no other components  to use these strucutre, we move them to podController.
	// If they are shared among other resources, we need lock whenever we access to them
	npMgr *NetworkPolicyManager
}

func NewPodController(podInformer coreinformer.PodInformer, clientset *kubernetes.Clientset, npMgr *NetworkPolicyManager) *podController {
	podController := &podController{
		clientset:       clientset,
		podLister:       podInformer.Lister(),
		podListerSynced: podInformer.Informer().HasSynced,
		workqueue:       workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "Pods"),
		npMgr:           npMgr,
	}

	podInformer.Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc:    podController.addPod,
			UpdateFunc: podController.updatePod,
			DeleteFunc: podController.deletePod,
		},
	)
	return podController
}

// filter this event if we do not need to handle this event
func (c *podController) needSync(obj interface{}) (string, string, bool) {
	needSync := false
	var podKey string
	var key string

	podObj, ok := obj.(*corev1.Pod)
	if !ok {
		metrics.SendErrorLogAndMetric(util.NpmID, "ADD Pod: Received unexpected object type: %v", obj)
		return podKey, key, needSync
	}

	// 1. To filter out invalid pod - bring checking code from syncAddedPod() function
	// Does it need to retry?
	if !isValidPod(podObj) {
		return podKey, key, needSync
	}

	// Ignore adding the HostNetwork pod to any ipsets.
	if isHostNetworkPod(podObj) {
		log.Logf("HostNetwork POD IGNORED: [%s/%s/%s/%+v%s]", podObj.GetObjectMeta().GetUID(), podObj.Namespace, podObj.Name, podObj.Labels, podObj.Status.PodIP)
		return podKey, key, needSync
	}

	// TODO: Is "GetPodKey" the same as "MetaNamespaceKeyFunc"?
	podKey = GetPodKey(podObj)
	if podKey == "" {
		err := fmt.Errorf("[AddPod] Error: podKey is empty for %s pod in %s with UID %s", podObj.Name, util.GetNSNameWithPrefix(podObj.Namespace), podObj.UID)
		metrics.SendErrorLogAndMetric(util.PodID, err.Error())
		return podKey, key, needSync
	}
	var err error
	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		utilruntime.HandleError(err)
		return podKey, key, needSync
	}

	needSync = true
	return podKey, key, needSync
}

func (c *podController) addPod(obj interface{}) {
	// TODO: What are the difference between podKey and key?
	//podKey, key, needSync := c.needSync(obj)
	_, key, needSync := c.needSync(obj)
	if !needSync {
		return
	}
	// K8s categorizes Succeeded abd Failed pods be terminated and will not restart them
	// So NPM will ignorer adding these pods

	// TODO: do we need to put below in needSync function?
	podObj, _ := obj.(*corev1.Pod)
	if podObj.Status.Phase == v1.PodSucceeded || podObj.Status.Phase == v1.PodFailed {
		return
	}

	// If the pod is ready to install ipset, put podKey into workqueue.
	c.workqueue.Add(key)
}

func (c *podController) updatePod(old, new interface{}) {
	// TODO: how to use podKey?
	//podKey, key, needSync := c.needSync(obj)
	_, key, needSync := c.needSync(new)
	if !needSync {
		return
	}

	// If the pod is ready to install ipset, put podKey into workqueue.
	c.workqueue.Add(key)
}

func (c *podController) deletePod(obj interface{}) {
	podObj, ok := obj.(*corev1.Pod)
	// TODO: not clear
	// DeleteFunc gets the final state of the resource (if it is known).
	// Otherwise, it gets an object of type DeletedFinalStateUnknown.
	// This can happen if the watch is closed and misses the delete event and
	// the controller doesn't notice the deletion until the subsequent re-list
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			metrics.SendErrorLogAndMetric(util.NpmID, "DELETE Pod: Received unexpected object type: %v", obj)
			return
		}
		if podObj, ok = tombstone.Obj.(*corev1.Pod); !ok {
			metrics.SendErrorLogAndMetric(util.NpmID, "DELETE Pod: Received unexpected object type: %v", obj)
			return
		}
	}

	if isHostNetworkPod(podObj) {
		return
	}

	podKey := GetPodKey(podObj)
	if podKey == "" {
		err := fmt.Errorf("[DeletePod] Error: podKey is empty for %s pod in %s with UID %s", podObj.ObjectMeta.Name, util.GetNSNameWithPrefix(podObj.Namespace), podObj.UID)
		metrics.SendErrorLogAndMetric(util.PodID, err.Error())
		return
	}

	// it should exists?
	cachedPodObj, podExists := c.npMgr.PodMap[podKey]
	if !podExists {
		return
	}

	// if the podIp exists, it must match the cachedIp
	if len(podObj.Status.PodIP) > 0 && cachedPodObj.PodIP != podObj.Status.PodIP {
		metrics.SendErrorLogAndMetric(util.PodID, "[DeletePod] Info: Unexpected state. Pod (Namespace:%s, Name:%s, uid:%s, has cachedPodIp:%s which is different from PodIp:%s",
			util.GetNSNameWithPrefix(podObj.Namespace), podObj.ObjectMeta.Name, podObj.UID, cachedPodObj.PodIP, podObj.Status.PodIP)
	}
	var key string
	var err error
	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		utilruntime.HandleError(err)
		return
	}

	// If the pod is ready to install ipset, put podKey into workqueue.
	c.workqueue.Add(key)
}

func (c *podController) Run(threadiness int, stopCh <-chan struct{}) error {
	defer utilruntime.HandleCrash()
	defer c.workqueue.ShutDown()

	// Start the informer factories to begin populating the informer caches
	log.Logf("Starting Pod controlle\n")

	// log.Logf("Waiting for informer caches to sync")
	// Junguk - sync all resources in npm together
	// if ok := cache.WaitForCacheSync(stopCh, c.podListerSynced); !ok {
	// 	return fmt.Errorf("failed to wait for caches to sync")
	// }

	log.Logf("Starting workers")
	// Launch two workers to process Pod resources
	for i := 0; i < threadiness; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	log.Logf("Started workers")
	<-stopCh
	log.Logf("Shutting down workers")

	return nil
}

func (c *podController) runWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *podController) processNextWorkItem() bool {
	obj, shutdown := c.workqueue.Get()

	if shutdown {
		return false
	}

	err := func(obj interface{}) error {
		defer c.workqueue.Done(obj)
		var key string
		var ok bool
		if key, ok = obj.(string); !ok {
			// As the item in the workqueue is actually invalid, we call
			// Forget here else we'd go into a loop of attempting to
			// process a work item that is invalid.
			c.workqueue.Forget(obj)
			utilruntime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
			return nil
		}
		// Run the syncHandler, passing it the namespace/name string of the
		// Pod resource to be synced.

		// TODO : can consider using "c.queue.AddAfter(key, *requeueAfter)" according to error type
		if err := c.syncHandler(key); err != nil {
			// Put the item back on the workqueue to handle any transient errors.
			c.workqueue.AddRateLimited(key)
			return fmt.Errorf("error syncing '%s': %s, requeuing", key, err.Error())
		}
		// Finally, if no error occurs we Forget this item so it does not
		// get queued again until another change happens.
		c.workqueue.Forget(obj)
		log.Logf("Successfully synced '%s'", key)
		return nil
	}(obj)

	if err != nil {
		utilruntime.HandleError(err)
		return true
	}

	return true
}

// syncHandler compares the actual state with the desired, and attempts to
// converge the two.
func (c *podController) syncHandler(key string) error {
	// Convert the namespace/name string into a distinct namespace and name
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}

	// Get the Pod resource with this namespace/name
	pod, err := c.podLister.Pods(namespace).Get(name)
	if err != nil {
		if errors.IsNotFound(err) {
			utilruntime.HandleError(fmt.Errorf("pod '%s' in work queue no longer exists", key))
			// find the pod object from a local cache and start cleaning up process (calling cleanUpDeletedPod function)
			cachedPod, found := c.podCache[key]
			if found {
				err = c.cleanUpDeletedPod(cachedPod)
				if err != nil {
					// cleaning process was failed, need to requeue and retry later.
					return fmt.Errorf("Cannot delete ipset due to %s\n", err.Error())
				}
				delete(c.podCache, key)
			}
			// for other transient apiserver error requeue with exponential backoff
			return err
		}
	}

	// TODO: do action for create and update events
	// install ipset, update, and delete ipset
	err = c.syncPod(pod)

	// 1. deal with error code and retry this
	if err != nil {
		return fmt.Errorf("Failed to sync pod due to  %s\n", err.Error())
	}

	return nil
}

// logic for resync pod (e.g., install ipset)
func (c *podController) syncPod(podObj *corev1.Pod) error {
	// if pod exists in cache, it is update event

	// TODO: need to know addEvent or updateEvent by using local cache?
	err := c.syncAddedPod(podObj)
	if err != nil {
		return err
	}
	return nil
}

// TODO: need to collapse it with syncUpdatedPod
func (c *podController) syncAddedPod(podObj *corev1.Pod) error {
	// TODO: do we need npmPodObj?
	npmPodObj, podErr := newNpmPod(podObj)
	if podErr != nil {
		metrics.SendErrorLogAndMetric(util.PodID, "[AddPod] Error: failed to create namespace %s, %+v with err %v", podObj.ObjectMeta.Name, podObj, podErr)
		return podErr
	}

	var (
		err               error
		podKey            = GetPodKey(podObj)
		podNs             = util.GetNSNameWithPrefix(npmPodObj.Namespace)
		podUID            = npmPodObj.PodUID
		podName           = npmPodObj.Name
		podNodeName       = npmPodObj.NodeName
		podLabels         = npmPodObj.Labels
		podIP             = npmPodObj.PodIP
		podContainerPorts = npmPodObj.ContainerPorts
		ipsMgr            = c.npMgr.NsMap[util.KubeAllNamespacesFlag].IpsMgr
	)

	log.Logf("POD CREATING: [%s%s/%s/%s%+v%s]", podUID, podNs, podName, podNodeName, podLabels, podIP)

	// Add pod namespace if it doesn't exist
	if _, exists := c.npMgr.NsMap[podNs]; !exists {
		c.npMgr.NsMap[podNs], err = newNs(podNs)
		if err != nil {
			metrics.SendErrorLogAndMetric(util.PodID, "[AddPod] Error: failed to create namespace %s with err: %v", podNs, err)
		}
		log.Logf("Creating set: %v, hashedSet: %v", podNs, util.GetHashedName(podNs))
		if err = ipsMgr.CreateSet(podNs, append([]string{util.IpsetNetHashFlag})); err != nil {
			metrics.SendErrorLogAndMetric(util.PodID, "[AddPod] Error: creating ipset %s with err: %v", podNs, err)
			return err
		}
	}

	// Add the pod to its namespace's ipset.
	log.Logf("Adding pod %s to ipset %s", podIP, podNs)
	if err = ipsMgr.AddToSet(podNs, podIP, util.IpsetNetHashFlag, podUID); err != nil {
		metrics.SendErrorLogAndMetric(util.PodID, "[AddPod] Error: failed to add pod to namespace ipset with err: %v", err)
		return err
	}

	// Add the pod to its label's ipset.
	for podLabelKey, podLabelVal := range podLabels {
		log.Logf("Adding pod %s to ipset %s", podIP, podLabelKey)
		if err = ipsMgr.AddToSet(podLabelKey, podIP, util.IpsetNetHashFlag, podUID); err != nil {
			metrics.SendErrorLogAndMetric(util.PodID, "[AddPod] Error: failed to add pod to label ipset with err: %v", err)
			return err
		}

		label := podLabelKey + ":" + podLabelVal
		log.Logf("Adding pod %s to ipset %s", podIP, label)
		if err = ipsMgr.AddToSet(label, podIP, util.IpsetNetHashFlag, podUID); err != nil {
			metrics.SendErrorLogAndMetric(util.PodID, "[AddPod] Error: failed to add pod to label ipset with err: %v", err)
			return err
		}
	}

	// Add pod's named ports from its ipset.
	if err = appendNamedPortIpsets(ipsMgr, podContainerPorts, podUID, podIP, false); err != nil {
		metrics.SendErrorLogAndMetric(util.PodID, "[AddPod] Error: failed to add pod to namespace ipset with err: %v", err)
		return err
	}

	// add the Pod info to the podMap
	c.npMgr.PodMap[podKey] = npmPodObj

	return nil
}

// UpdatePod handles updating pod ip in its label's ipset.
func (c *podController) syncUpdatePod(newPodObj *corev1.Pod) error {
	var (
		err            error
		podKey         = GetPodKey(newPodObj)
		newPodObjNs    = util.GetNSNameWithPrefix(newPodObj.ObjectMeta.Namespace)
		newPodObjName  = newPodObj.ObjectMeta.Name
		newPodObjLabel = newPodObj.ObjectMeta.Labels
		newPodObjPhase = newPodObj.Status.Phase
		newPodObjIP    = newPodObj.Status.PodIP
		ipsMgr         = c.npMgr.NsMap[util.KubeAllNamespacesFlag].IpsMgr
	)

	// Add pod namespace if it doesn't exist
	if _, exists := c.npMgr.NsMap[newPodObjNs]; !exists {
		c.npMgr.NsMap[newPodObjNs], err = newNs(newPodObjNs)
		if err != nil {
			metrics.SendErrorLogAndMetric(util.PodID, "[UpdatePod] Error: failed to create namespace %s with err: %v", newPodObjNs, err)
		}
		log.Logf("Creating set: %v, hashedSet: %v", newPodObjNs, util.GetHashedName(newPodObjNs))
		if err = ipsMgr.CreateSet(newPodObjNs, append([]string{util.IpsetNetHashFlag})); err != nil {
			metrics.SendErrorLogAndMetric(util.PodID, "[UpdatePod] Error creating ipset %s with err: %v", newPodObjNs, err)
			return err
		}
	}

	cachedPodObj, exists := c.npMgr.PodMap[podKey]
	if !exists {
		if addErr := c.syncAddedPod(newPodObj); addErr != nil {
			//if addErr := c.npMgr.AddPod(newPodObj); addErr != nil {
			metrics.SendErrorLogAndMetric(util.PodID, "[UpdatePod] Error: failed to add pod during update with error %+v", addErr)
			return addErr
		}
		return nil
	}

	if isInvalidPodUpdate(getPodObjFromNpmObj(cachedPodObj), newPodObj) {
		return nil
	}

	check := util.CompareUintResourceVersions(
		cachedPodObj.ResourceVersion,
		util.ParseResourceVersion(newPodObj.ObjectMeta.ResourceVersion),
	)
	if !check {
		log.Logf(
			"POD UPDATING ignored as resourceVersion of cached pod is greater Pod:\n cached pod: [%s/%s/%s/%d]\n new pod: [%s/%s/%s/%s]",
			cachedPodObj.Namespace, cachedPodObj.Name, cachedPodObj.PodIP, cachedPodObj.ResourceVersion,
			newPodObj.ObjectMeta.Namespace, newPodObj.ObjectMeta.Name, newPodObj.Status.PodIP, newPodObj.ObjectMeta.ResourceVersion,
		)

		return nil
	}

	// We are assuming that FAILED to RUNNING pod will send an update
	if newPodObj.Status.Phase == v1.PodSucceeded || newPodObj.Status.Phase == v1.PodFailed {
		if delErr := c.cleanUpDeletedPod(newPodObj); delErr != nil {
			metrics.SendErrorLogAndMetric(util.PodID, "[UpdatePod] Error: failed to add pod during update with error %+v", delErr)
			return delErr
		}

		return nil
	}

	var (
		cachedPodIP  = cachedPodObj.PodIP
		cachedLabels = cachedPodObj.Labels
	)

	log.Logf(
		"POD UPDATING:\n new pod: [%s/%s/%+v/%s/%s]\n cached pod: [%s/%s/%+v/%s]",
		newPodObjNs, newPodObjName, newPodObjLabel, newPodObjPhase, newPodObjIP,
		cachedPodObj.Namespace, cachedPodObj.Name, cachedPodObj.Labels, cachedPodObj.PodIP,
	)

	deleteFromIPSets := []string{}
	addToIPSets := []string{}

	// if the podIp exists, it must match the cachedIp
	if cachedPodIP != newPodObjIP {
		metrics.SendErrorLogAndMetric(util.PodID, "[UpdatePod] Info: Unexpected state. Pod (Namespace:%s, Name:%s, uid:%s, has cachedPodIp:%s which is different from PodIp:%s",
			newPodObjNs, newPodObjName, cachedPodObj.PodUID, cachedPodIP, newPodObjIP)
		// cached PodIP needs to be cleaned up from all the cached labels
		deleteFromIPSets = util.GetIPSetListFromLabels(cachedLabels)

		// Assume that the pod IP will be released when pod moves to succeeded or failed state.
		// If the pod transitions back to an active state, then add operation will re establish the updated pod info.
		// new PodIP needs to be added to all newLabels
		addToIPSets = util.GetIPSetListFromLabels(newPodObjLabel)

		// Delete the pod from its namespace's ipset.
		log.Logf("Deleting pod %s %s from ipset %s and adding pod %s to ipset %s",
			cachedPodObj.PodUID, cachedPodIP, cachedPodObj.Namespace, newPodObjIP, newPodObjNs,
		)
		if err = ipsMgr.DeleteFromSet(cachedPodObj.Namespace, cachedPodIP, cachedPodObj.PodUID); err != nil {
			metrics.SendErrorLogAndMetric(util.PodID, "[UpdatePod] Error: failed to delete pod from namespace ipset with err: %v", err)
			return err
		}
		// Add the pod to its namespace's ipset.
		if err = ipsMgr.AddToSet(newPodObjNs, newPodObjIP, util.IpsetNetHashFlag, cachedPodObj.PodUID); err != nil {
			metrics.SendErrorLogAndMetric(util.PodID, "[UpdatePod] Error: failed to add pod to namespace ipset with err: %v", err)
			return err
		}
	} else {
		//if no change in labels then return
		if reflect.DeepEqual(cachedLabels, newPodObjLabel) {
			log.Logf(
				"POD UPDATING:\n nothing to delete or add. pod: [%s/%s]",
				newPodObjNs, newPodObjName,
			)
			return nil
		}
		// delete PodIP from removed labels and add PodIp to new labels
		addToIPSets, deleteFromIPSets = util.GetIPSetListCompareLabels(cachedLabels, newPodObjLabel)
	}

	// Delete the pod from its label's ipset.
	for _, podIPSetName := range deleteFromIPSets {
		log.Logf("Deleting pod %s from ipset %s", cachedPodIP, podIPSetName)
		if err = ipsMgr.DeleteFromSet(podIPSetName, cachedPodIP, cachedPodObj.PodUID); err != nil {
			metrics.SendErrorLogAndMetric(util.PodID, "[UpdatePod] Error: failed to delete pod from label ipset with err: %v", err)
			return err
		}
	}

	// Add the pod to its label's ipset.
	for _, addIPSetName := range addToIPSets {
		log.Logf("Adding pod %s to ipset %s", newPodObjIP, addIPSetName)
		if err = ipsMgr.AddToSet(addIPSetName, newPodObjIP, util.IpsetNetHashFlag, cachedPodObj.PodUID); err != nil {
			metrics.SendErrorLogAndMetric(util.PodID, "[UpdatePod] Error: failed to add pod to label ipset with err: %v", err)
			return err
		}
	}

	// TODO optimize named port addition and deletions.
	// named ports are mostly static once configured in todays usage pattern
	// so keeping this simple by deleting all and re-adding
	newPodPorts := getContainerPortList(newPodObj)
	if !reflect.DeepEqual(cachedPodObj.ContainerPorts, newPodPorts) {
		// Delete cached pod's named ports from its ipset.
		if err = appendNamedPortIpsets(ipsMgr, cachedPodObj.ContainerPorts, cachedPodObj.PodUID, cachedPodIP, true); err != nil {
			metrics.SendErrorLogAndMetric(util.PodID, "[UpdatePod] Error: failed to delete pod from namespace ipset with err: %v", err)
			return err
		}
		// Add new pod's named ports from its ipset.
		if err = appendNamedPortIpsets(ipsMgr, newPodPorts, cachedPodObj.PodUID, newPodObjIP, false); err != nil {
			metrics.SendErrorLogAndMetric(util.PodID, "[UpdatePod] Error: failed to add pod to namespace ipset with err: %v", err)
			return err
		}
	}

	// Updating pod cache with new information
	c.npMgr.PodMap[podKey], err = newNpmPod(newPodObj)
	if err != nil {
		return err
	}

	return nil
}

// DeletePod handles deleting pod from its label's ipset.
func (c *podController) cleanUpDeletedPod(podObj *corev1.Pod) error {
	podNs := util.GetNSNameWithPrefix(podObj.Namespace)
	var err error
	podKey := GetPodKey(podObj)
	ipsMgr := c.npMgr.NsMap[util.KubeAllNamespacesFlag].IpsMgr
	podUID := string(podObj.ObjectMeta.UID)

	cachedPodObj, _ := c.npMgr.PodMap[podKey]
	cachedPodIP := cachedPodObj.PodIP
	podLabels := cachedPodObj.Labels
	containerPorts := cachedPodObj.ContainerPorts

	// Delete the pod from its namespace's ipset.
	if err = ipsMgr.DeleteFromSet(podNs, cachedPodIP, podUID); err != nil {
		metrics.SendErrorLogAndMetric(util.PodID, "[DeletePod] Error: failed to delete pod from namespace ipset with err: %v", err)
		return err
	}

	// Delete the pod from its label's ipset.
	for podLabelKey, podLabelVal := range podLabels {
		log.Logf("Deleting pod %s from ipset %s", cachedPodIP, podLabelKey)
		if err = ipsMgr.DeleteFromSet(podLabelKey, cachedPodIP, podUID); err != nil {
			metrics.SendErrorLogAndMetric(util.PodID, "[DeletePod] Error: failed to delete pod from label ipset with err: %v", err)
			return err
		}

		label := podLabelKey + ":" + podLabelVal
		log.Logf("Deleting pod %s from ipset %s", cachedPodIP, label)
		if err = ipsMgr.DeleteFromSet(label, cachedPodIP, podUID); err != nil {
			metrics.SendErrorLogAndMetric(util.PodID, "[DeletePod] Error: failed to delete pod from label ipset with err: %v", err)
			return err
		}
	}

	// Delete pod's named ports from its ipset. Delete is TRUE
	if err = appendNamedPortIpsets(ipsMgr, containerPorts, podUID, cachedPodIP, true); err != nil {
		metrics.SendErrorLogAndMetric(util.PodID, "[DeletePod] Error: failed to delete pod from namespace ipset with err: %v", err)
		return err
	}

	delete(c.npMgr.PodMap, podKey)

	return nil
}
