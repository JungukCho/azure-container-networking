package npm

import (
	"os"
	"testing"

	"github.com/Azure/azure-container-networking/npm/ipsm"
	"github.com/Azure/azure-container-networking/npm/iptm"
	"github.com/Azure/azure-container-networking/npm/metrics"
	"github.com/Azure/azure-container-networking/npm/util"
	"k8s.io/client-go/tools/cache"
)

// To indicate the object is needed to be DeletedFinalStateUnknown Object
type IsDeletedFinalStateUnknownObject bool

const (
	DeletedFinalStateUnknownObject IsDeletedFinalStateUnknownObject = true
	DeletedFinalStateknownObject   IsDeletedFinalStateUnknownObject = false
)

func getKey(obj interface{}, t *testing.T) string {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		t.Errorf("Unexpected error getting key for obj %v: %v", obj, err)
		return ""
	}
	return key
}

func newNPMgr(t *testing.T) *NetworkPolicyManager {
	npMgr := &NetworkPolicyManager{
		NsMap:            make(map[string]*Namespace),
		TelemetryEnabled: false,
	}

	// This initialization important as without this NPM will panic
	allNs := newNs(util.KubeAllNamespacesFlag)
	npMgr.NsMap[util.KubeAllNamespacesFlag] = allNs
	return npMgr
}

func TestMain(m *testing.M) {
	metrics.InitializeAll()
	iptMgr := iptm.NewIptablesManager()
	iptMgr.Save(util.IptablesConfigFile)

	ipsMgr := ipsm.NewIpsetManager()
	ipsMgr.Save(util.IpsetConfigFile)

	exitCode := m.Run()

	iptMgr.Restore(util.IptablesConfigFile)
	ipsMgr.Restore(util.IpsetConfigFile)

	os.Exit(exitCode)
}
