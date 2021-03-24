// Copyright 2018 Microsoft. All rights reserved.
// MIT License
package npm

import (
	"fmt"
	"strconv"
	"time"

	"github.com/Azure/azure-container-networking/log"
	"github.com/Azure/azure-container-networking/npm/ipsm"
	"github.com/Azure/azure-container-networking/npm/iptm"
	"github.com/Azure/azure-container-networking/npm/metrics"
	"github.com/Azure/azure-container-networking/npm/util"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	networkinginformers "k8s.io/client-go/informers/networking/v1"
	"k8s.io/client-go/kubernetes"
	netpollister "k8s.io/client-go/listers/networking/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

// TODO
// 1. put lock
// 2. walk through codes in details
// 3. clean up functions
type netPolController struct {
	clientset          *kubernetes.Clientset
	netPolLister       netpollister.NetworkPolicyLister
	netPolListerSynced cache.InformerSynced

	workqueue workqueue.RateLimitingInterface
	// TODO: netPolController does not need to have whole NetworkPolicyManager pointer. Need to improve it
	npMgr *NetworkPolicyManager
}

func NewNetPolController(npInformer networkinginformers.NetworkPolicyInformer, clientset *kubernetes.Clientset, npMgr *NetworkPolicyManager) *netPolController {
	netPolController := &netPolController{
		clientset:          clientset,
		netPolLister:       npInformer.Lister(),
		netPolListerSynced: npInformer.Informer().HasSynced,
		workqueue:          workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "Pods"),
		npMgr:              npMgr,
	}

	npInformer.Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc:    netPolController.addNetPol,
			UpdateFunc: netPolController.updateNetPol,
			DeleteFunc: netPolController.deleteNetPol,
		},
	)
	return netPolController
}

// filter this event if we do not need to handle this event
func (c *netPolController) needSync(obj interface{}) (string, bool) {
	needSync := false
	var key string

	npObj, ok := obj.(*networkingv1.NetworkPolicy)
	if !ok {
		metrics.SendErrorLogAndMetric(util.NpmID, "Need sync for NetPol : Received unexpected object type: %v", obj)
		return key, needSync
	}

	var err error
	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		utilruntime.HandleError(err)
		err = fmt.Errorf("[needSync] Error: Network Policy is empty for %s network policy in %s with UID %s", npObj.Name, npObj.Namespace, npObj.UID)
		metrics.SendErrorLogAndMetric(util.PodID, err.Error())
		return key, needSync
	}

	needSync = true
	return key, needSync
}

func (c *netPolController) addNetPol(obj interface{}) {
	log.Logf("[NETPOL ADD EVENT]")
	key, needSync := c.needSync(obj)
	if !needSync {
		log.Logf("[NETPOL ADD EVENT] No need to sync this network policy")
		return
	}
	c.workqueue.Add(key)
}

func (c *netPolController) updateNetPol(old, new interface{}) {
	log.Logf("[NETPOL UPDATE EVENT]")
	oldNpObj, ok := old.(*networkingv1.NetworkPolicy)
	if !ok {
		metrics.SendErrorLogAndMetric(util.NpmID, "UPDATE Network Policy: Received unexpected old object type: %v", oldNpObj)
		return
	}
	newNpObj, ok := new.(*networkingv1.NetworkPolicy)
	if !ok {
		metrics.SendErrorLogAndMetric(util.NpmID, "UPDATE Network Policy: Received unexpected new object type: %v", newNpObj)
		return
	}

	if oldNpObj.ObjectMeta.DeletionTimestamp != nil || newNpObj.ObjectMeta.DeletionGracePeriodSeconds == nil {
		return
	}

	// (Question) - Why? - change if condition to return it.
	// if oldNpObj.ObjectMeta.DeletionTimestamp == nil && newNpObj.ObjectMeta.DeletionGracePeriodSeconds == nil {
	// }

	key, needSync := c.needSync(new)
	if !needSync {
		log.Logf("[NETPOL UPDATE EVENT] No need to sync this network policy")
		return
	}

	c.workqueue.Add(key)
}

func (c *netPolController) deleteNetPol(obj interface{}) {
	log.Logf("[NETPOL DELETE EVENT]")
	netPol, ok := obj.(*networkingv1.NetworkPolicy)
	// DeleteFunc gets the final state of the resource (if it is known).
	// Otherwise, it gets an object of type DeletedFinalStateUnknown.
	// This can happen if the watch is closed and misses the delete event and
	// the controller doesn't notice the deletion until the subsequent re-list
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			metrics.SendErrorLogAndMetric(util.NpmID, "DELETE Network Policy: Received unexpected object type: %v", obj)
			return
		}

		if netPol, ok = tombstone.Obj.(*networkingv1.NetworkPolicy); !ok {
			metrics.SendErrorLogAndMetric(util.NpmID, "DELETE Network Policy: Received unexpected object type (error decoding object tombstone, invalid type): %v", obj)
			return
		}
	}

	var err error
	var key string
	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		utilruntime.HandleError(err)
		err := fmt.Errorf("[DeletePod] Error: Network Policy is empty for %s NetPol in %s with UID %s", netPol.Name, netPol.Namespace, netPol.UID)
		metrics.SendErrorLogAndMetric(util.PodID, err.Error())
		return
	}

	c.workqueue.Add(key)
}

func (c *netPolController) Run(threadiness int, stopCh <-chan struct{}) error {
	defer utilruntime.HandleCrash()
	defer c.workqueue.ShutDown()

	// Start the informer factories to begin populating the informer caches
	log.Logf("Starting NetPol controller\n")

	log.Logf("Starting NetPol workers")
	// Launch two workers to process Pod resources
	for i := 0; i < threadiness; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	log.Logf("Started NetPol workers")
	<-stopCh
	log.Logf("Shutting down NetPol workers")

	return nil
}

func (c *netPolController) runWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *netPolController) processNextWorkItem() bool {
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
		if err := c.syncNetPol(key); err != nil {
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
func (c *netPolController) syncNetPol(key string) error {
	// Convert the namespace/name string into a distinct namespace and name
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}

	// Get the network policy resource with this namespace/name
	netPol, err := c.netPolLister.NetworkPolicies(namespace).Get(name)
	// lock to complete events
	// TODO: Reduce scope of lock later
	c.npMgr.Lock()
	defer c.npMgr.Unlock()
	if err != nil {
		if errors.IsNotFound(err) {
			utilruntime.HandleError(fmt.Errorf("NetPol '%s' in work queue no longer exists", key))
			err = c.deleteNetworkPolicy(netPol)
			if err != nil {
				// cleaning process was failed, need to requeue and retry later.
				return fmt.Errorf("Cannot clean up Network Policy due to %s\n", err.Error())
			}
		}
		// for other transient apiserver error requeue with exponential backoff
		// TODO: check this..
		return err
	}

	err = c.syncAddAndUpdateNetPol(netPol)
	// 1. deal with error code and retry this
	if err != nil {
		return fmt.Errorf("Failed to sync network policy due to  %s\n", err.Error())
	}

	return nil
}

// DeleteNetworkPolicy handles deleting network policy from iptables.
func (c *netPolController) deleteNetworkPolicy(npObj *networkingv1.NetworkPolicy) error {
	var (
		err            error
		allNs          = c.npMgr.NsMap[util.KubeAllNamespacesFlag]
		hashedSelector = HashSelector(&npObj.Spec.PodSelector)
		npKey          = c.getNetworkPolicyKey(npObj)
		npProcessedKey = c.getProcessedNPKey(npObj, hashedSelector)
	)

	npNs, npName := util.GetNSNameWithPrefix(npObj.ObjectMeta.Namespace), npObj.ObjectMeta.Name
	log.Logf("NETWORK POLICY DELETING: Namespace: %s, Name:%s", npNs, npName)

	if npKey == "" {
		err = fmt.Errorf("[cleanUpDeletedNetPol] Error: npKey is empty for %s network policy in %s", npName, npNs)
		metrics.SendErrorLogAndMetric(util.NetpolID, err.Error())
		return err
	}

	_, _, _, ingressIPCidrs, egressIPCidrs, iptEntries := translatePolicy(npObj)

	iptMgr := allNs.iptMgr
	for _, iptEntry := range iptEntries {
		if err = iptMgr.Delete(iptEntry); err != nil {
			metrics.SendErrorLogAndMetric(util.NetpolID, "[DeleteNetworkPolicy] Error: failed to apply iptables rule. Rule: %+v with err: %v", iptEntry, err)
			return err
		}
	}

	err = c.removeCidrsRule("in", npObj.ObjectMeta.Name, npObj.ObjectMeta.Namespace, ingressIPCidrs, allNs.IpsMgr)
	if err != nil {
		return err
	}

	err = c.removeCidrsRule("out", npObj.ObjectMeta.Name, npObj.ObjectMeta.Namespace, egressIPCidrs, allNs.IpsMgr)
	if err != nil {
		return err
	}

	delete(c.npMgr.RawNpMap, npKey)

	if oldPolicy, oldPolicyExists := c.npMgr.ProcessedNpMap[npProcessedKey]; oldPolicyExists {
		deductedPolicy, err := deductPolicy(oldPolicy, npObj)
		if err != nil {
			metrics.SendErrorLogAndMetric(util.NetpolID, "[DeleteNetworkPolicy] Error: deducting policy %s from %s with err: %v", npName, oldPolicy.ObjectMeta.Name, err)
			return err
		}

		if deductedPolicy == nil {
			delete(c.npMgr.ProcessedNpMap, npProcessedKey)
		} else {
			c.npMgr.ProcessedNpMap[npProcessedKey] = deductedPolicy
		}
	}

	if c.npMgr.canCleanUpNpmChains() {
		c.npMgr.isAzureNpmChainCreated = false
		if err = iptMgr.UninitNpmChains(); err != nil {
			metrics.SendErrorLogAndMetric(util.NetpolID, "[DeleteNetworkPolicy] Error: failed to uninitialize azure-npm chains with err: %s", err)
			return err
		}
	}

	metrics.NumPolicies.Dec()
	return nil
}

// Add and Update NetworkPolicy handles adding network policy to iptables.
func (c *netPolController) syncAddAndUpdateNetPol(npObj *networkingv1.NetworkPolicy) error {
	var (
		err            error
		ns             *Namespace
		exists         bool
		npNs           = util.GetNSNameWithPrefix(npObj.ObjectMeta.Namespace)
		npName         = npObj.ObjectMeta.Name
		allNs          = c.npMgr.NsMap[util.KubeAllNamespacesFlag]
		timer          = metrics.StartNewTimer()
		hashedSelector = HashSelector(&npObj.Spec.PodSelector)
		npKey          = c.getNetworkPolicyKey(npObj)
		npProcessedKey = c.getProcessedNPKey(npObj, hashedSelector)
	)

	log.Logf("NETWORK POLICY CREATING: NameSpace%s, Name:%s", npNs, npName)

	if npKey == "" {
		err = fmt.Errorf("[AddNetworkPolicy] Error: npKey is empty for %s network policy in %s", npName, npNs)
		metrics.SendErrorLogAndMetric(util.NetpolID, err.Error())
		return err
	}

	if ns, exists = c.npMgr.NsMap[npNs]; !exists {
		ns, err = newNs(npNs)
		if err != nil {
			metrics.SendErrorLogAndMetric(util.NetpolID, "[AddNetworkPolicy] Error: creating namespace %s with err: %v", npNs, err)
		}
		c.npMgr.NsMap[npNs] = ns
	}

	if c.policyExists(npObj) {
		return nil
	}

	if !c.npMgr.isAzureNpmChainCreated {
		if err = allNs.IpsMgr.CreateSet(util.KubeSystemFlag, append([]string{util.IpsetNetHashFlag})); err != nil {
			metrics.SendErrorLogAndMetric(util.NetpolID, "[AddNetworkPolicy] Error: failed to initialize kube-system ipset with err %s", err)
			return err
		}

		if err = allNs.iptMgr.InitNpmChains(); err != nil {
			metrics.SendErrorLogAndMetric(util.NetpolID, "[AddNetworkPolicy] Error: failed to initialize azure-npm chains with err %s", err)
			return err
		}

		c.npMgr.isAzureNpmChainCreated = true
	}

	var (
		addedPolicy                   *networkingv1.NetworkPolicy
		sets, namedPorts, lists       []string
		ingressIPCidrs, egressIPCidrs [][]string
		iptEntries                    []*iptm.IptEntry
		ipsMgr                        = allNs.IpsMgr
	)

	// Remove the existing policy from processed (merged) network policy map
	if oldPolicy, oldPolicyExists := c.npMgr.RawNpMap[npKey]; oldPolicyExists {
		c.npMgr.isSafeToCleanUpAzureNpmChain = false
		c.deleteNetworkPolicy(oldPolicy)
		c.npMgr.isSafeToCleanUpAzureNpmChain = true
	}

	// Add (merge) the new policy with others who apply to the same pods
	if oldPolicy, oldPolicyExists := c.npMgr.ProcessedNpMap[npProcessedKey]; oldPolicyExists {
		addedPolicy, err = addPolicy(oldPolicy, npObj)
		if err != nil {
			metrics.SendErrorLogAndMetric(util.NetpolID, "[AddNetworkPolicy] Error: adding policy %s to %s with err: %v", npName, oldPolicy.ObjectMeta.Name, err)
			return err
		}
	}

	if addedPolicy != nil {
		c.npMgr.ProcessedNpMap[npProcessedKey] = addedPolicy
	} else {
		c.npMgr.ProcessedNpMap[npProcessedKey] = npObj
	}

	c.npMgr.RawNpMap[npKey] = npObj

	sets, namedPorts, lists, ingressIPCidrs, egressIPCidrs, iptEntries = translatePolicy(npObj)
	for _, set := range sets {
		log.Logf("Creating set: %v, hashedSet: %v", set, util.GetHashedName(set))
		if err = ipsMgr.CreateSet(set, append([]string{util.IpsetNetHashFlag})); err != nil {
			metrics.SendErrorLogAndMetric(util.NetpolID, "[AddNetworkPolicy] Error: creating ipset %s with err: %v", set, err)
			return err
		}
	}
	for _, set := range namedPorts {
		log.Logf("Creating set: %v, hashedSet: %v", set, util.GetHashedName(set))
		if err = ipsMgr.CreateSet(set, append([]string{util.IpsetIPPortHashFlag})); err != nil {
			metrics.SendErrorLogAndMetric(util.NetpolID, "[AddNetworkPolicy] Error: creating ipset named port %s with err: %v", set, err)
			return err
		}
	}
	for _, list := range lists {
		if err = ipsMgr.CreateList(list); err != nil {
			metrics.SendErrorLogAndMetric(util.NetpolID, "[AddNetworkPolicy] Error: creating ipset list %s with err: %v", list, err)
			return err
		}
	}
	if err = c.npMgr.InitAllNsList(); err != nil {
		metrics.SendErrorLogAndMetric(util.NetpolID, "[AddNetworkPolicy] Error: initializing all-namespace ipset list with err: %v", err)
		return err
	}

	// need to deal with error
	err = c.createCidrsRule("in", npObj.ObjectMeta.Name, npObj.ObjectMeta.Namespace, ingressIPCidrs, ipsMgr)
	if err != nil {
		return err
	}

	c.createCidrsRule("out", npObj.ObjectMeta.Name, npObj.ObjectMeta.Namespace, egressIPCidrs, ipsMgr)
	if err != nil {
		return err
	}

	iptMgr := allNs.iptMgr
	for _, iptEntry := range iptEntries {
		if err = iptMgr.Add(iptEntry); err != nil {
			metrics.SendErrorLogAndMetric(util.NetpolID, "[AddNetworkPolicy] Error: failed to apply iptables rule. Rule: %+v with err: %v", iptEntry, err)
			return err
		}
	}

	metrics.NumPolicies.Inc()
	timer.StopAndRecord(metrics.AddPolicyExecTime)

	return nil
}

func (c *netPolController) createCidrsRule(ingressOrEgress, policyName, ns string, ipsetEntries [][]string, ipsMgr *ipsm.IpsetManager) error {
	spec := append([]string{util.IpsetNetHashFlag, util.IpsetMaxelemName, util.IpsetMaxelemNum})

	for i, ipCidrSet := range ipsetEntries {
		if ipCidrSet == nil || len(ipCidrSet) == 0 {
			continue
		}
		setName := policyName + "-in-ns-" + ns + "-" + strconv.Itoa(i) + ingressOrEgress
		log.Logf("Creating set: %v, hashedSet: %v", setName, util.GetHashedName(setName))
		if err := ipsMgr.CreateSet(setName, spec); err != nil {
			metrics.SendErrorLogAndMetric(util.NetpolID, "[createCidrsRule] Error: creating ipset %s with err: %v", ipCidrSet, err)
		}
		for _, ipCidrEntry := range util.DropEmptyFields(ipCidrSet) {
			// Ipset doesn't allow 0.0.0.0/0 to be added. A general solution is split 0.0.0.0/1 in half which convert to
			// 1.0.0.0/1 and 128.0.0.0/1
			if ipCidrEntry == "0.0.0.0/0" {
				splitEntry := [2]string{"1.0.0.0/1", "128.0.0.0/1"}
				for _, entry := range splitEntry {
					if err := ipsMgr.AddToSet(setName, entry, util.IpsetNetHashFlag, ""); err != nil {
						metrics.SendErrorLogAndMetric(util.NetpolID, "[createCidrsRule] adding ip cidrs %s into ipset %s with err: %v", entry, ipCidrSet, err)
						return err
					}
				}
			} else {
				if err := ipsMgr.AddToSet(setName, ipCidrEntry, util.IpsetNetHashFlag, ""); err != nil {
					metrics.SendErrorLogAndMetric(util.NetpolID, "[createCidrsRule] adding ip cidrs %s into ipset %s with err: %v", ipCidrEntry, ipCidrSet, err)
					return err
				}
			}
		}
	}

	return nil
}

func (c *netPolController) removeCidrsRule(ingressOrEgress, policyName, ns string, ipsetEntries [][]string, ipsMgr *ipsm.IpsetManager) error {
	for i, ipCidrSet := range ipsetEntries {
		if ipCidrSet == nil || len(ipCidrSet) == 0 {
			continue
		}
		setName := policyName + "-in-ns-" + ns + "-" + strconv.Itoa(i) + ingressOrEgress
		log.Logf("Delete set: %v, hashedSet: %v", setName, util.GetHashedName(setName))
		if err := ipsMgr.DeleteSet(setName); err != nil {
			metrics.SendErrorLogAndMetric(util.NetpolID, "[removeCidrsRule] deleting ipset %s with err: %v", ipCidrSet, err)
			return err
		}
	}

	return nil
}

func (c *netPolController) policyExists(npObj *networkingv1.NetworkPolicy) bool {
	npKey := c.getNetworkPolicyKey(npObj)
	if npKey == "" {
		return false
	}

	np, exists := c.npMgr.RawNpMap[npKey]
	if !exists {
		return false
	}

	if !util.CompareResourceVersions(np.ObjectMeta.ResourceVersion, npObj.ObjectMeta.ResourceVersion) {
		log.Logf("Cached Network Policy has larger ResourceVersion number than new Obj. Name: %s Cached RV: %d New RV: %d\n",
			npObj.ObjectMeta.Name,
			np.ObjectMeta.ResourceVersion,
			npObj.ObjectMeta.ResourceVersion,
		)
		return true
	}

	if isSamePolicy(np, npObj) {
		return true
	}

	return false
}

// GetNetworkPolicyKey will return netpolKey
func (c *netPolController) getNetworkPolicyKey(npObj *networkingv1.NetworkPolicy) string {
	netpolKey, err := util.GetObjKeyFunc(npObj)
	if err != nil {
		metrics.SendErrorLogAndMetric(util.NetpolID, "[GetNetworkPolicyKey] Error: while running MetaNamespaceKeyFunc err: %s", err)
		return ""
	}
	if len(netpolKey) == 0 {
		return ""
	}
	return util.GetNSNameWithPrefix(netpolKey)
}

// GetProcessedNPKey will return netpolKey
func (c *netPolController) getProcessedNPKey(npObj *networkingv1.NetworkPolicy, hashSelector string) string {
	// hashSelector will never be empty
	netpolKey := hashSelector
	if len(npObj.GetNamespace()) > 0 {
		netpolKey = npObj.GetNamespace() + "/" + netpolKey
	}
	return util.GetNSNameWithPrefix(netpolKey)
}

func (npMgr *NetworkPolicyManager) canCleanUpNpmChains() bool {
	if !npMgr.isSafeToCleanUpAzureNpmChain {
		return false
	}

	if len(npMgr.ProcessedNpMap) > 0 {
		return false
	}

	return true
}
