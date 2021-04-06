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
	clientset       *kubernetes.Clientset
	podLister       corelisters.PodLister
	podListerSynced cache.InformerSynced
	workqueue       workqueue.RateLimitingInterface
	//podCache        map[string]*v1.Pod
	// TODO: podController does not need to have whole NetworkPolicyManager pointer. Need to improve it
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
	log.Logf("Starting Pod controller\n")

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
			utilruntime.HandleError(fmt.Errorf("pod '%s' in work queue no longer exists", key))
			// find the pod object from a local cache and start cleaning up process (calling cleanUpDeletedPod function)
			cachedPod, found := c.npMgr.PodMap[key]
			if found {
				// TODO: better to use cachedPod when calling cleanUpDeletedPod?
				err = c.cleanUpDeletedPod(getPodObjFromNpmObj(cachedPod))
				if err != nil {
					// cleaning process was failed, need to requeue and retry later.
					return fmt.Errorf("Cannot delete ipset due to %s\n", err.Error())
				}
			}
		}
		return err
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
	podKey, _ := cache.MetaNamespaceKeyFunc(podObj)
	var (
		err               error
		podNs             = util.GetNSNameWithPrefix(podObj.Namespace)
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
		// TODO: Any chance to return error?
		c.npMgr.NsMap[podNs], _ = newNs(podNs)
		log.Logf("Creating set: %v, hashedSet: %v", podNs, util.GetHashedName(podNs))
		if err = ipsMgr.CreateSet(podNs, append([]string{util.IpsetNetHashFlag})); err != nil {
			metrics.SendErrorLogAndMetric(util.PodID, "[AddPod] Error: creating ipset %s with err: %v", podNs, err)
			return err
		}
	}

	// Add the pod to its namespace's ipset.
	log.Logf("Adding pod %s to ipset %s", podIP, podNs)
	if err = ipsMgr.AddToSet(podNs, podIP, util.IpsetNetHashFlag, podKey); err != nil {
		metrics.SendErrorLogAndMetric(util.PodID, "[AddPod] Error: failed to add pod to namespace ipset with err: %v", err)
		return err
	}

	// Add the pod to its label's ipset.
	for podLabelKey, podLabelVal := range podLabels {
		log.Logf("Adding pod %s to ipset %s", podIP, podLabelKey)
		if err = ipsMgr.AddToSet(podLabelKey, podIP, util.IpsetNetHashFlag, podKey); err != nil {
			metrics.SendErrorLogAndMetric(util.PodID, "[AddPod] Error: failed to add pod to label ipset with err: %v", err)
			return err
		}

		label := podLabelKey + ":" + podLabelVal
		log.Logf("Adding pod %s to ipset %s", podIP, label)
		if err = ipsMgr.AddToSet(label, podIP, util.IpsetNetHashFlag, podKey); err != nil {
			metrics.SendErrorLogAndMetric(util.PodID, "[AddPod] Error: failed to add pod to label ipset with err: %v", err)
			return err
		}
	}

	// Add pod's named ports from its ipset.
	if err = appendNamedPortIpsets(ipsMgr, podContainerPorts, podKey, podIP, false); err != nil {
		metrics.SendErrorLogAndMetric(util.PodID, "[AddPod] Error: failed to add pod to namespace ipset with err: %v", err)
		return err
	}

	// add the Pod info to the podMap
	c.npMgr.PodMap[podKey] = npmPodObj
	return nil
}

// UpdatePod handles updating pod ip in its label's ipset.
func (c *podController) syncAddAndUpdatePod(newPodObj *corev1.Pod) error {
	log.Logf("[syncAddAndUpdatePod]")
	podKey, _ := cache.MetaNamespaceKeyFunc(newPodObj)

	var (
		err            error
		newPodObjNs    = util.GetNSNameWithPrefix(newPodObj.ObjectMeta.Namespace)
		newPodObjName  = newPodObj.ObjectMeta.Name
		newPodObjLabel = newPodObj.ObjectMeta.Labels
		newPodObjPhase = newPodObj.Status.Phase
		newPodObjIP    = newPodObj.Status.PodIP
		ipsMgr         = c.npMgr.NsMap[util.KubeAllNamespacesFlag].IpsMgr
	)

	// Add pod namespace if it doesn't exist
	if _, exists := c.npMgr.NsMap[newPodObjNs]; !exists {
		// TODO: Any chance to return error?
		c.npMgr.NsMap[newPodObjNs], _ = newNs(newPodObjNs)
		log.Logf("Creating set: %v, hashedSet: %v", newPodObjNs, util.GetHashedName(newPodObjNs))
		if err = ipsMgr.CreateSet(newPodObjNs, append([]string{util.IpsetNetHashFlag})); err != nil {
			metrics.SendErrorLogAndMetric(util.PodID, "[UpdatePod] Error creating ipset %s with err: %v", newPodObjNs, err)
			return err
		}
	}

	cachedPodObj, exists := c.npMgr.PodMap[podKey]
	if !exists {
		if addErr := c.syncAddedPod(newPodObj); addErr != nil {
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

	log.Logf("POD UPDATING:\n new pod: [%s/%s/%+v/%s/%s]\n cached pod: [%s/%s/%+v/%s]",
		newPodObjNs, newPodObjName, newPodObjLabel, newPodObjPhase, newPodObjIP,
		cachedPodObj.Namespace, cachedPodObj.Name, cachedPodObj.Labels, cachedPodObj.PodIP,
	)

	cachedPodIP := cachedPodObj.PodIP
	cachedLabels := cachedPodObj.Labels
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
		if err = ipsMgr.DeleteFromSet(cachedPodObj.Namespace, cachedPodIP, podKey); err != nil {
			metrics.SendErrorLogAndMetric(util.PodID, "[UpdatePod] Error: failed to delete pod from namespace ipset with err: %v", err)
			return err
		}
		// Add the pod to its namespace's ipset.
		if err = ipsMgr.AddToSet(newPodObjNs, newPodObjIP, util.IpsetNetHashFlag, podKey); err != nil {
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
		if err = ipsMgr.DeleteFromSet(podIPSetName, cachedPodIP, podKey); err != nil {
			metrics.SendErrorLogAndMetric(util.PodID, "[UpdatePod] Error: failed to delete pod from label ipset with err: %v", err)
			return err
		}
	}

	// Add the pod to its label's ipset.
	for _, addIPSetName := range addToIPSets {
		log.Logf("Adding pod %s to ipset %s", newPodObjIP, addIPSetName)
		if err = ipsMgr.AddToSet(addIPSetName, newPodObjIP, util.IpsetNetHashFlag, podKey); err != nil {
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
		if err = appendNamedPortIpsets(ipsMgr, cachedPodObj.ContainerPorts, podKey, cachedPodIP, true); err != nil {
			metrics.SendErrorLogAndMetric(util.PodID, "[UpdatePod] Error: failed to delete pod from namespace ipset with err: %v", err)
			return err
		}
		// Add new pod's named ports from its ipset.
		if err = appendNamedPortIpsets(ipsMgr, newPodPorts, podKey, newPodObjIP, false); err != nil {
			metrics.SendErrorLogAndMetric(util.PodID, "[UpdatePod] Error: failed to add pod to namespace ipset with err: %v", err)
			return err
		}
	}

	// Updating pod cache with new information
	// TODO: Any chance to return error?
	c.npMgr.PodMap[podKey], _ = newNpmPod(newPodObj)
	return nil
}

// DeletePod handles deleting pod from its label's ipset.
func (c *podController) cleanUpDeletedPod(podObj *corev1.Pod) error {
	log.Logf("[cleanUpDeletedPod]")
	podNs := util.GetNSNameWithPrefix(podObj.Namespace)
	podKey, _ := cache.MetaNamespaceKeyFunc(podObj)
	ipsMgr := c.npMgr.NsMap[util.KubeAllNamespacesFlag].IpsMgr
	cachedPodObj, _ := c.npMgr.PodMap[podKey]
	cachedPodIP := cachedPodObj.PodIP
	podLabels := cachedPodObj.Labels
	containerPorts := cachedPodObj.ContainerPorts

	// Delete the pod from its namespace's ipset.
	var err error
	if err = ipsMgr.DeleteFromSet(podNs, cachedPodIP, podKey); err != nil {
		metrics.SendErrorLogAndMetric(util.PodID, "[DeletePod] Error: failed to delete pod from namespace ipset with err: %v", err)
		return err
	}

	// Delete the pod from its label's ipset.
	for podLabelKey, podLabelVal := range podLabels {
		log.Logf("Deleting pod %s from ipset %s", cachedPodIP, podLabelKey)
		if err = ipsMgr.DeleteFromSet(podLabelKey, cachedPodIP, podKey); err != nil {
			metrics.SendErrorLogAndMetric(util.PodID, "[DeletePod] Error: failed to delete pod from label ipset with err: %v", err)
			return err
		}

		label := podLabelKey + ":" + podLabelVal
		log.Logf("Deleting pod %s from ipset %s", cachedPodIP, label)
		if err = ipsMgr.DeleteFromSet(label, cachedPodIP, podKey); err != nil {
			metrics.SendErrorLogAndMetric(util.PodID, "[DeletePod] Error: failed to delete pod from label ipset with err: %v", err)
			return err
		}
	}

	// Delete pod's named ports from its ipset. Delete is TRUE
	if err = appendNamedPortIpsets(ipsMgr, containerPorts, podKey, cachedPodIP, true); err != nil {
		metrics.SendErrorLogAndMetric(util.PodID, "[DeletePod] Error: failed to delete pod from namespace ipset with err: %v", err)
		return err
	}

	delete(c.npMgr.PodMap, podKey)
	return nil
}
