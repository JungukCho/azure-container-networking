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

// getPodObjFromNpmObj returns a new pod object based on NpmPod
func (nPod *NpmPod) getPodObjFromNpmObj() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      nPod.Name,
			Namespace: nPod.Namespace,
			Labels:    nPod.Labels,
			UID:       types.UID(nPod.PodUID),
		},
		Status: corev1.PodStatus{
			Phase:  nPod.Phase,
			PodIP:  nPod.PodIP,
			PodIPs: nPod.PodIPs,
		},
		Spec: corev1.PodSpec{
			HostNetwork: nPod.IsHostNetwork,
			NodeName:    nPod.NodeName,
			Containers: []v1.Container{
				v1.Container{
					Ports: nPod.ContainerPorts,
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

func isInvalidPodUpdate(oldPodObj, newPodObj *corev1.Pod) bool {
	isInvalidUpdate := oldPodObj.ObjectMeta.Namespace == newPodObj.ObjectMeta.Namespace &&
		oldPodObj.ObjectMeta.Name == newPodObj.ObjectMeta.Name &&
		oldPodObj.Status.Phase == newPodObj.Status.Phase &&
		oldPodObj.Status.PodIP == newPodObj.Status.PodIP &&
		newPodObj.ObjectMeta.DeletionTimestamp == nil &&
		newPodObj.ObjectMeta.DeletionGracePeriodSeconds == nil
	isInvalidUpdate = isInvalidUpdate &&
		reflect.DeepEqual(oldPodObj.ObjectMeta.Labels, newPodObj.ObjectMeta.Labels) &&
		reflect.DeepEqual(oldPodObj.Status.PodIPs, newPodObj.Status.PodIPs) &&
		reflect.DeepEqual(getContainerPortList(oldPodObj), getContainerPortList(newPodObj))

	return isInvalidUpdate
}

func getContainerPortList(podObj *corev1.Pod) []v1.ContainerPort {
	portList := []v1.ContainerPort{}
	for _, container := range podObj.Spec.Containers {
		portList = append(portList, container.Ports...)
	}
	return portList
}

// appendNamedPortIpsets helps with adding or deleting Pod namedPort IPsets
func appendNamedPortIpsets(ipsMgr *ipsm.IpsetManager, portList []v1.ContainerPort, podKey string, podIP string, delete bool) error {
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
				podKey,
			)
			continue
		}
		// Add the pod's named ports to its ipset.
		ipsMgr.AddToSet(
			namedPortname,
			fmt.Sprintf("%s,%s%d", podIP, protocol, port.ContainerPort),
			util.IpsetIPPortHashFlag,
			podKey,
		)
	}
	return nil
}

type podController struct {
	clientset       kubernetes.Interface
	podLister       corelisters.PodLister
	podListerSynced cache.InformerSynced
	workqueue       workqueue.RateLimitingInterface
	//podCache        map[string]*v1.Pod
	// TODO: podController does not need to have whole NetworkPolicyManager pointer. Need to improve it
	npMgr *NetworkPolicyManager
}

func NewPodController(podInformer coreinformer.PodInformer, clientset kubernetes.Interface, npMgr *NetworkPolicyManager) *podController {
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
func (c *podController) needSync(eventType string, obj interface{}) (string, bool) {
	needSync := false
	var key string

	podObj, ok := obj.(*corev1.Pod)
	if !ok {
		metrics.SendErrorLogAndMetric(util.NpmID, "ADD Pod: Received unexpected object type: %v", obj)
		return key, needSync
	}

	log.Logf("[POD %s EVENT for %s in %s", eventType, podObj.Name, podObj.Namespace)

	if !isValidPod(podObj) {
		return key, needSync
	}

	// Ignore adding the HostNetwork pod to any ipsets.
	if isHostNetworkPod(podObj) {
		log.Logf("[POD %s EVENT] HostNetwork POD IGNORED: [%s/%s/%s/%+v%s]", eventType, podObj.GetObjectMeta().GetUID(), podObj.Namespace, podObj.Name, podObj.Labels, podObj.Status.PodIP)
		return key, needSync
	}

	var err error
	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		utilruntime.HandleError(err)
		err = fmt.Errorf("[POD %s EVENT] Error: podKey is empty for %s pod in %s with UID %s", eventType, podObj.Name, util.GetNSNameWithPrefix(podObj.Namespace), podObj.UID)
		metrics.SendErrorLogAndMetric(util.PodID, err.Error())
		return key, needSync
	}

	needSync = true
	return key, needSync
}

func (c *podController) addPod(obj interface{}) {
	key, needSync := c.needSync("ADD", obj)
	if !needSync {
		log.Logf("[POD ADD EVENT] No need to sync this pod")
		return
	}
	// K8s categorizes Succeeded abd Failed pods be terminated and will not restart them
	// So NPM will ignorer adding these pods
	podObj, _ := obj.(*corev1.Pod)
	if podObj.Status.Phase == v1.PodSucceeded || podObj.Status.Phase == v1.PodFailed {
		return
	}
	c.workqueue.Add(key)
}

func (c *podController) updatePod(old, new interface{}) {
	oldPod := old.(*corev1.Pod)
	newPod := new.(*corev1.Pod)
	if oldPod.ResourceVersion == newPod.ResourceVersion {
		// Periodic resync will send update events for all known pods.
		// Two different versions of the same pods will always have different RVs.
		// will check version..
		fmt.Println("[POD UPDATE EVENT] two pods have the same RVs")
		return
	}

	key, needSync := c.needSync("UPDATE", new)
	if !needSync {
		log.Logf("[POD UPDATE EVENT] No need to sync this pod")
		return
	}
	c.workqueue.Add(key)
}

func (c *podController) deletePod(obj interface{}) {
	podObj, ok := obj.(*corev1.Pod)
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
			metrics.SendErrorLogAndMetric(util.NpmID, "DELETE Pod: Received unexpected object type (error decoding object tombstone, invalid type): %v", obj)
			return
		}
	}

	log.Logf("[POD DELETE EVENT for %s in %s", podObj.Name, podObj.Namespace)

	if isHostNetworkPod(podObj) {
		log.Logf("[POD DELETE EVENT] HostNetwork POD IGNORED: [%s/%s/%s/%+v%s]", podObj.UID, podObj.Namespace, podObj.Name, podObj.Labels, podObj.Status.PodIP)
		return
	}

	var err error
	var key string
	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		utilruntime.HandleError(err)
		err := fmt.Errorf("[DeletePod] Error: podKey is empty for %s pod in %s with UID %s", podObj.ObjectMeta.Name, util.GetNSNameWithPrefix(podObj.Namespace), podObj.UID)
		metrics.SendErrorLogAndMetric(util.PodID, err.Error())
		return
	}

	c.npMgr.Lock()
	defer c.npMgr.Unlock()

	cachedPodObj, podExists := c.npMgr.PodMap[key]
	if !podExists {
		return
	}

	// if the podIp exists, it must match the cachedIp
	// TODO: should we return this error?
	if len(podObj.Status.PodIP) > 0 && cachedPodObj.PodIP != podObj.Status.PodIP {
		metrics.SendErrorLogAndMetric(util.PodID, "[DeletePod] Info: Unexpected state. Pod (Namespace:%s, Name:%s, uid:%s, has cachedPodIp:%s which is different from PodIp:%s",
			util.GetNSNameWithPrefix(podObj.Namespace), podObj.ObjectMeta.Name, podObj.UID, cachedPodObj.PodIP, podObj.Status.PodIP)
	}

	c.workqueue.Add(key)
}

func (c *podController) Run(threadiness int, stopCh <-chan struct{}) error {
	defer utilruntime.HandleCrash()
	defer c.workqueue.ShutDown()

	// Start the informer factories to begin populating the informer caches
	log.Logf("Starting Pod workers")
	// Launch two workers to process Pod resources
	for i := 0; i < threadiness; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	log.Logf("Started Pod workers")
	<-stopCh
	log.Logf("Shutting down Pod workers")

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

	log.Logf("Length of queue %d", c.workqueue.Len())

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
		// TODO : may consider using "c.queue.AddAfter(key, *requeueAfter)" according to error type later
		if err := c.syncPod(key); err != nil {
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

// syncPod compares the actual state with the desired, and attempts to converge the two.
func (c *podController) syncPod(key string) error {
	// Convert the namespace/name string into a distinct namespace and name
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}

	// Get the Pod resource with this namespace/name
	pod, err := c.podLister.Pods(namespace).Get(name)
	// lock to complete events
	// TODO: Reduce scope of lock later
	c.npMgr.Lock()
	defer c.npMgr.Unlock()
	if err != nil {
		if errors.IsNotFound(err) {
			fmt.Printf("pod %s not found, may be it is deleted\n", key)
			// Find the pod object from a local cache and start cleaning up process (calling cleanUpDeletedPod function)
			cachedPod, found := c.npMgr.PodMap[key]
			if found {
				// TODO: better to use cachedPod when calling cleanUpDeletedPod?
				err = c.cleanUpDeletedPod(cachedPod.getPodObjFromNpmObj())
				if err != nil {
					// cleaning process was failed, need to requeue and retry later.
					return fmt.Errorf("Cannot delete ipset due to %s\n", err.Error())
				}
			}
		}
		return err
	}

	if pod.DeletionTimestamp != nil || pod.DeletionGracePeriodSeconds != nil {
		err = c.cleanUpDeletedPod(pod)
		if err != nil {
			return fmt.Errorf("Failed to clean up pod due to  %s\n", err.Error())
		}
	}

	err = c.syncAddAndUpdatePod(pod)
	// 1. deal with error code and retry this
	if err != nil {
		return fmt.Errorf("Failed to sync pod due to  %s\n", err.Error())
	}

	return nil
}

func (c *podController) syncAddedPod(podObj *corev1.Pod) error {
	// TODO: Any chance to return error?
	npmPodObj, _ := newNpmPod(podObj)
	podNs := util.GetNSNameWithPrefix(podObj.Namespace)
	podKey, _ := cache.MetaNamespaceKeyFunc(podObj)
	ipsMgr := c.npMgr.NsMap[util.KubeAllNamespacesFlag].IpsMgr
	log.Logf("POD CREATING: [%s%s/%s/%s%+v%s]", npmPodObj.PodUID, podNs, npmPodObj.Name, npmPodObj.NodeName, npmPodObj.Labels, npmPodObj.PodIP)

	// Add pod namespace if it doesn't exist
	var err error
	if _, exists := c.npMgr.NsMap[podNs]; !exists {
		// TODO: Any chance to return error?
		c.npMgr.NsMap[podNs], _ = newNs(podNs)
		log.Logf("Creating set: %v, hashedSet: %v", podNs, util.GetHashedName(podNs))
		if err = ipsMgr.CreateSet(podNs, append([]string{util.IpsetNetHashFlag})); err != nil {
			metrics.SendErrorLogAndMetric(util.PodID, "[AddPod] Error: creating ipset %s with err: %v", podNs, err)
			return err
		}
	}

	// Add the pod to its namespace's ipset.
	log.Logf("Adding pod %s to ipset %s", npmPodObj.PodIP, podNs)
	if err = ipsMgr.AddToSet(podNs, npmPodObj.PodIP, util.IpsetNetHashFlag, podKey); err != nil {
		metrics.SendErrorLogAndMetric(util.PodID, "[AddPod] Error: failed to add pod to namespace ipset with err: %v", err)
		return err
	}

	// Add the pod to its label's ipset.
	for podLabelKey, podLabelVal := range npmPodObj.Labels {
		log.Logf("Adding pod %s to ipset %s", npmPodObj.PodIP, podLabelKey)
		if err = ipsMgr.AddToSet(podLabelKey, npmPodObj.PodIP, util.IpsetNetHashFlag, podKey); err != nil {
			metrics.SendErrorLogAndMetric(util.PodID, "[AddPod] Error: failed to add pod to label ipset with err: %v", err)
			return err
		}

		label := podLabelKey + ":" + podLabelVal
		log.Logf("Adding pod %s to ipset %s", npmPodObj.PodIP, label)
		if err = ipsMgr.AddToSet(label, npmPodObj.PodIP, util.IpsetNetHashFlag, podKey); err != nil {
			metrics.SendErrorLogAndMetric(util.PodID, "[AddPod] Error: failed to add pod to label ipset with err: %v", err)
			return err
		}
	}

	// Add pod's named ports from its ipset.
	if err = appendNamedPortIpsets(ipsMgr, npmPodObj.ContainerPorts, podKey, npmPodObj.PodIP, false); err != nil {
		metrics.SendErrorLogAndMetric(util.PodID, "[AddPod] Error: failed to add pod to namespace ipset with err: %v", err)
		return err
	}

	// add the Pod info to the podMap
	c.npMgr.PodMap[podKey] = npmPodObj
	return nil
}

// syncAddAndUpdatePod handles updating pod ip in its label's ipset.
func (c *podController) syncAddAndUpdatePod(newPodObj *corev1.Pod) error {
	log.Logf("[syncAddAndUpdatePod]")

	podKey, _ := cache.MetaNamespaceKeyFunc(newPodObj)
	newPodObjNs := util.GetNSNameWithPrefix(newPodObj.ObjectMeta.Namespace)
	ipsMgr := c.npMgr.NsMap[util.KubeAllNamespacesFlag].IpsMgr

	// Add pod namespace if it doesn't exist
	var err error
	if _, exists := c.npMgr.NsMap[newPodObjNs]; !exists {
		// TODO: Any chance to return error?
		c.npMgr.NsMap[newPodObjNs], _ = newNs(newPodObjNs)
		log.Logf("Creating set: %v, hashedSet: %v", newPodObjNs, util.GetHashedName(newPodObjNs))
		if err = ipsMgr.CreateSet(newPodObjNs, append([]string{util.IpsetNetHashFlag})); err != nil {
			metrics.SendErrorLogAndMetric(util.PodID, "[UpdatePod] Error creating ipset %s with err: %v", newPodObjNs, err)
			return err
		}
	}

	cachedNpmPodObj, exists := c.npMgr.PodMap[podKey]
	if !exists {
		if addErr := c.syncAddedPod(newPodObj); addErr != nil {
			metrics.SendErrorLogAndMetric(util.PodID, "[UpdatePod] Error: failed to add pod during update with error %+v", addErr)
			return addErr
		}
		log.Logf("[Stop processing since it is pod creation event]")
		return nil
	}

	// Below logic is to deal with update event
	if isInvalidPodUpdate(cachedNpmPodObj.getPodObjFromNpmObj(), newPodObj) {
		return nil
	}

	// TODO: does it always increase? - Unnecessary?
	check := util.CompareUintResourceVersions(
		cachedNpmPodObj.ResourceVersion,
		util.ParseResourceVersion(newPodObj.ObjectMeta.ResourceVersion),
	)
	if !check {
		log.Logf(
			"POD UPDATING ignored as resourceVersion of cached pod is greater Pod:\n cached pod: [%s/%s/%s/%d]\n new pod: [%s/%s/%s/%s]",
			cachedNpmPodObj.Namespace, cachedNpmPodObj.Name, cachedNpmPodObj.PodIP, cachedNpmPodObj.ResourceVersion,
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

	log.Logf("POD UPDATING:\n new pod: [%s/%s/%+v/%s/%s]\n cached pod: [%s/%s/%+v/%s]",
		newPodObjNs, newPodObj.Name, newPodObj.Labels, newPodObj.Status.Phase, newPodObj.Status.PodIP,
		cachedNpmPodObj.Namespace, cachedNpmPodObj.Name, cachedNpmPodObj.Labels, cachedNpmPodObj.PodIP,
	)

	deleteFromIPSets := []string{}
	addToIPSets := []string{}
	// if the podIp exists, it must match the cachedIp
	if cachedNpmPodObj.PodIP != newPodObj.Status.PodIP {
		metrics.SendErrorLogAndMetric(util.PodID, "[UpdatePod] Info: Unexpected state. Pod (Namespace:%s, Name:%s, uid:%s, has cachedPodIp:%s which is different from PodIp:%s",
			newPodObjNs, newPodObj.Name, cachedNpmPodObj.PodUID, cachedNpmPodObj.PodIP, newPodObj.Status.PodIP)
		// cached PodIP needs to be cleaned up from all the cached labels
		deleteFromIPSets = util.GetIPSetListFromLabels(cachedNpmPodObj.Labels)

		// Assume that the pod IP will be released when pod moves to succeeded or failed state.
		// If the pod transitions back to an active state, then add operation will re establish the updated pod info.
		// new PodIP needs to be added to all newLabels
		addToIPSets = util.GetIPSetListFromLabels(newPodObj.Labels)

		// Delete the pod from its namespace's ipset.
		log.Logf("Deleting pod %s %s from ipset %s and adding pod %s to ipset %s",
			cachedNpmPodObj.PodUID, cachedNpmPodObj.PodIP, cachedNpmPodObj.Namespace, newPodObj.Status.PodIP, newPodObjNs,
		)
		if err = ipsMgr.DeleteFromSet(cachedNpmPodObj.Namespace, cachedNpmPodObj.PodIP, podKey); err != nil {
			metrics.SendErrorLogAndMetric(util.PodID, "[UpdatePod] Error: failed to delete pod from namespace ipset with err: %v", err)
			return err
		}
		// Add the pod to its namespace's ipset.
		if err = ipsMgr.AddToSet(newPodObjNs, newPodObj.Status.PodIP, util.IpsetNetHashFlag, podKey); err != nil {
			metrics.SendErrorLogAndMetric(util.PodID, "[UpdatePod] Error: failed to add pod to namespace ipset with err: %v", err)
			return err
		}
	} else {
		//if no change in labels then return
		if reflect.DeepEqual(cachedNpmPodObj.Labels, newPodObj.Labels) {
			log.Logf(
				"POD UPDATING:\n nothing to delete or add. pod: [%s/%s]",
				newPodObjNs, newPodObj.Name,
			)
			return nil
		}
		// delete PodIP from removed labels and add PodIp to new labels
		addToIPSets, deleteFromIPSets = util.GetIPSetListCompareLabels(cachedNpmPodObj.Labels, newPodObj.Labels)
	}

	// Delete the pod from its label's ipset.
	for _, podIPSetName := range deleteFromIPSets {
		log.Logf("Deleting pod %s from ipset %s", cachedNpmPodObj.PodIP, podIPSetName)
		if err = ipsMgr.DeleteFromSet(podIPSetName, cachedNpmPodObj.PodIP, podKey); err != nil {
			metrics.SendErrorLogAndMetric(util.PodID, "[UpdatePod] Error: failed to delete pod from label ipset with err: %v", err)
			return err
		}
	}

	// Add the pod to its label's ipset.
	for _, addIPSetName := range addToIPSets {
		log.Logf("Adding pod %s to ipset %s", newPodObj.Status.PodIP, addIPSetName)
		if err = ipsMgr.AddToSet(addIPSetName, newPodObj.Status.PodIP, util.IpsetNetHashFlag, podKey); err != nil {
			metrics.SendErrorLogAndMetric(util.PodID, "[UpdatePod] Error: failed to add pod to label ipset with err: %v", err)
			return err
		}
	}

	// TODO optimize named port addition and deletions.
	// named ports are mostly static once configured in todays usage pattern
	// so keeping this simple by deleting all and re-adding
	newPodPorts := getContainerPortList(newPodObj)
	if !reflect.DeepEqual(cachedNpmPodObj.ContainerPorts, newPodPorts) {
		// Delete cached pod's named ports from its ipset.
		if err = appendNamedPortIpsets(ipsMgr, cachedNpmPodObj.ContainerPorts, podKey, cachedNpmPodObj.PodIP, true); err != nil {
			metrics.SendErrorLogAndMetric(util.PodID, "[UpdatePod] Error: failed to delete pod from namespace ipset with err: %v", err)
			return err
		}
		// Add new pod's named ports from its ipset.
		if err = appendNamedPortIpsets(ipsMgr, newPodPorts, podKey, newPodObj.Status.PodIP, false); err != nil {
			metrics.SendErrorLogAndMetric(util.PodID, "[UpdatePod] Error: failed to add pod to namespace ipset with err: %v", err)
			return err
		}
	}

	// Updating pod cache with new information
	// TODO: Any chance to return error?
	c.npMgr.PodMap[podKey], _ = newNpmPod(newPodObj)
	return nil
}

// cleanUpDeletedPod cleans up all ipset associated with this pod
func (c *podController) cleanUpDeletedPod(podObj *corev1.Pod) error {
	log.Logf("[cleanUpDeletedPod]")

	podNs := util.GetNSNameWithPrefix(podObj.Namespace)
	podKey, _ := cache.MetaNamespaceKeyFunc(podObj)
	ipsMgr := c.npMgr.NsMap[util.KubeAllNamespacesFlag].IpsMgr
	cachedNpmPodObj, _ := c.npMgr.PodMap[podKey]

	var err error
	// Delete the pod from its namespace's ipset.
	if err = ipsMgr.DeleteFromSet(podNs, cachedNpmPodObj.PodIP, podKey); err != nil {
		metrics.SendErrorLogAndMetric(util.PodID, "[DeletePod] Error: failed to delete pod from namespace ipset with err: %v", err)
		return err
	}

	// Delete the pod from its label's ipset.
	for podLabelKey, podLabelVal := range cachedNpmPodObj.Labels {
		log.Logf("Deleting pod %s from ipset %s", cachedNpmPodObj.PodIP, podLabelKey)
		if err = ipsMgr.DeleteFromSet(podLabelKey, cachedNpmPodObj.PodIP, podKey); err != nil {
			metrics.SendErrorLogAndMetric(util.PodID, "[DeletePod] Error: failed to delete pod from label ipset with err: %v", err)
			return err
		}

		label := podLabelKey + ":" + podLabelVal
		log.Logf("Deleting pod %s from ipset %s", cachedNpmPodObj.PodIP, label)
		if err = ipsMgr.DeleteFromSet(label, cachedNpmPodObj.PodIP, podKey); err != nil {
			metrics.SendErrorLogAndMetric(util.PodID, "[DeletePod] Error: failed to delete pod from label ipset with err: %v", err)
			return err
		}
	}

	// Delete pod's named ports from its ipset. Need to pass true in the appendNamedPortIpsets function call
	if err = appendNamedPortIpsets(ipsMgr, cachedNpmPodObj.ContainerPorts, podKey, cachedNpmPodObj.PodIP, true); err != nil {
		metrics.SendErrorLogAndMetric(util.PodID, "[DeletePod] Error: failed to delete pod from namespace ipset with err: %v", err)
		return err
	}

	delete(c.npMgr.PodMap, podKey)
	return nil
}
