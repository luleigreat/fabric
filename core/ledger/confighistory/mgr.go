/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package confighistory

import (
	"fmt"

	"github.com/golang/protobuf/proto"
	"github.com/hyperledger/fabric/common/flogging"
	"github.com/hyperledger/fabric/core/ledger"
	"github.com/hyperledger/fabric/core/ledger/ledgerconfig"
	"github.com/hyperledger/fabric/protos/common"
	"github.com/hyperledger/fabric/protos/ledger/rwset/kvrwset"
	"github.com/pkg/errors"
)

var logger = flogging.MustGetLogger("confighistory")

const (
	collectionConfigNamespace = "lscc" // lscc namespace was introduced in version 1.2 and we continue to use this in order to be compatible with existing data
)

// Mgr should be registered as a state listener. The state listener builds the history and retriver helps in querying the history
type Mgr interface {
	ledger.StateListener
	GetRetriever(ledgerID string, ledgerInfoRetriever LedgerInfoRetriever) ledger.ConfigHistoryRetriever
	Close()
}

type mgr struct {
	ccInfoProvider ledger.DeployedChaincodeInfoProvider
	dbProvider     *dbProvider
}

// NewMgr constructs an instance that implements interface `Mgr`
func NewMgr(ccInfoProvider ledger.DeployedChaincodeInfoProvider) Mgr {
	return newMgr(ccInfoProvider, dbPath())
}

func newMgr(ccInfoProvider ledger.DeployedChaincodeInfoProvider, dbPath string) Mgr {
	return &mgr{ccInfoProvider, newDBProvider(dbPath)}
}

// InterestedInNamespaces implements function from the interface ledger.StateListener
func (m *mgr) InterestedInNamespaces() []string {
	return m.ccInfoProvider.Namespaces()
}

// StateCommitDone implements function from the interface ledger.StateListener
func (m *mgr) StateCommitDone(ledgerID string) {
	// Noop
}

// HandleStateUpdates implements function from the interface ledger.StateListener
// In this implementation, the latest collection config package is retrieved via
// ledger.DeployedChaincodeInfoProvider and is persisted as a separate entry in a separate db.
// The composite key for the entry is a tuple of <blockNum, namespace, key>
func (m *mgr) HandleStateUpdates(trigger *ledger.StateUpdateTrigger) error {
	updatedCCs, err := m.ccInfoProvider.UpdatedChaincodes(convertToKVWrites(trigger.StateUpdates))
	if err != nil {
		return err
	}
	if len(updatedCCs) == 0 {
		logger.Errorf("Config history manager is expected to recieve events only if at least one chaincode is updated stateUpdates = %#v",
			trigger.StateUpdates)
		return nil
	}
	updatedCollConfigs := map[string]*common.CollectionConfigPackage{}
	for _, cc := range updatedCCs {
		ccInfo, err := m.ccInfoProvider.ChaincodeInfo(trigger.LedgerID, cc.Name, trigger.PostCommitQueryExecutor)
		if err != nil {
			return err
		}
		if ccInfo.ExplicitCollectionConfigPkg == nil {
			continue
		}
		updatedCollConfigs[ccInfo.Name] = ccInfo.ExplicitCollectionConfigPkg
	}
	if len(updatedCollConfigs) == 0 {
		return nil
	}
	batch, err := prepareDBBatch(updatedCollConfigs, trigger.CommittingBlockNum)
	if err != nil {
		return err
	}
	dbHandle := m.dbProvider.getDB(trigger.LedgerID)
	return dbHandle.writeBatch(batch, true)
}

// GetRetriever returns an implementation of `ledger.ConfigHistoryRetriever` for the given ledger id.
func (m *mgr) GetRetriever(ledgerID string, ledgerInfoRetriever LedgerInfoRetriever) ledger.ConfigHistoryRetriever {
	return &retriever{
		ledgerInfoRetriever:    ledgerInfoRetriever,
		ledgerID:               ledgerID,
		deployedCCInfoProvider: m.ccInfoProvider,
		dbHandle:               m.dbProvider.getDB(ledgerID),
	}
}

// Close implements the function in the interface 'Mgr'
func (m *mgr) Close() {
	m.dbProvider.Close()
}

type retriever struct {
	ledgerInfoRetriever    LedgerInfoRetriever
	ledgerID               string
	deployedCCInfoProvider ledger.DeployedChaincodeInfoProvider
	dbHandle               *db
}

// MostRecentCollectionConfigBelow implements function from the interface ledger.ConfigHistoryRetriever
func (r *retriever) MostRecentCollectionConfigBelow(blockNum uint64, chaincodeName string) (*ledger.CollectionConfigInfo, error) {
	compositeKV, err := r.dbHandle.mostRecentEntryBelow(blockNum, collectionConfigNamespace, constructCollectionConfigKey(chaincodeName))
	if err != nil {
		return nil, err
	}
	qe, err := r.ledgerInfoRetriever.NewQueryExecutor()
	if err != nil {
		return nil, err
	}
	defer qe.Done()
	return addImplicitCollections(compositeKV, r.ledgerID, chaincodeName, qe, r.deployedCCInfoProvider)
}

// CollectionConfigAt implements function from the interface ledger.ConfigHistoryRetriever
func (r *retriever) CollectionConfigAt(blockNum uint64, chaincodeName string) (*ledger.CollectionConfigInfo, error) {
	info, err := r.ledgerInfoRetriever.GetBlockchainInfo()
	if err != nil {
		return nil, err
	}
	maxCommittedBlockNum := info.Height - 1
	if maxCommittedBlockNum < blockNum {
		return nil, &ledger.ErrCollectionConfigNotYetAvailable{MaxBlockNumCommitted: maxCommittedBlockNum,
			Msg: fmt.Sprintf("The maximum block number committed [%d] is less than the requested block number [%d]", maxCommittedBlockNum, blockNum)}
	}

	compositeKV, err := r.dbHandle.entryAt(blockNum, collectionConfigNamespace, constructCollectionConfigKey(chaincodeName))
	if err != nil {
		return nil, err
	}
	qe, err := r.ledgerInfoRetriever.NewQueryExecutor()
	if err != nil {
		return nil, err
	}
	defer qe.Done()
	return addImplicitCollections(compositeKV, r.ledgerID, chaincodeName, qe, r.deployedCCInfoProvider)
}

func prepareDBBatch(chaincodeCollConfigs map[string]*common.CollectionConfigPackage, committingBlockNum uint64) (*batch, error) {
	batch := newBatch()
	for ccName, collConfig := range chaincodeCollConfigs {
		key := constructCollectionConfigKey(ccName)
		var configBytes []byte
		var err error
		if configBytes, err = proto.Marshal(collConfig); err != nil {
			return nil, errors.WithStack(err)
		}
		batch.add(collectionConfigNamespace, key, committingBlockNum, configBytes)
	}
	return batch, nil
}

func compositeKVToCollectionConfig(compositeKV *compositeKV) (*ledger.CollectionConfigInfo, error) {
	conf := &common.CollectionConfigPackage{}
	if err := proto.Unmarshal(compositeKV.value, conf); err != nil {
		return nil, errors.Wrap(err, "error unmarshalling compositeKV to collection config")
	}
	return &ledger.CollectionConfigInfo{CollectionConfig: conf, CommittingBlockNum: compositeKV.blockNum}, nil
}

func constructCollectionConfigKey(chaincodeName string) string {
	return chaincodeName + "~collection" // collection config key as in version 1.2 and we continue to use this in order to be compatible with existing data
}

func dbPath() string {
	return ledgerconfig.GetConfigHistoryPath()
}

func convertToKVWrites(stateUpdates ledger.StateUpdates) map[string][]*kvrwset.KVWrite {
	m := map[string][]*kvrwset.KVWrite{}
	for ns, updates := range stateUpdates {
		m[ns] = updates.([]*kvrwset.KVWrite)
	}
	return m
}

func addImplicitCollections(
	compositeKV *compositeKV,
	lgrID, ccName string, qe ledger.SimpleQueryExecutor,
	p ledger.DeployedChaincodeInfoProvider,
) (*ledger.CollectionConfigInfo, error) {
	var collConf *ledger.CollectionConfigInfo
	var err error
	var implicitColls []*common.StaticCollectionConfig
	if implicitColls, err = p.ImplicitCollections(lgrID, ccName, qe); err != nil {
		return nil, err
	}
	if compositeKV == nil && len(implicitColls) == 0 {
		return nil, nil
	}

	collConf = &ledger.CollectionConfigInfo{
		CollectionConfig: &common.CollectionConfigPackage{},
	}
	if compositeKV != nil {
		if collConf, err = compositeKVToCollectionConfig(compositeKV); err != nil {
			return nil, err
		}
	}

	for _, implicitColl := range implicitColls {
		cc := &common.CollectionConfig{}
		cc.Payload = &common.CollectionConfig_StaticCollectionConfig{StaticCollectionConfig: implicitColl}
		collConf.CollectionConfig.Config = append(
			collConf.CollectionConfig.Config,
			cc,
		)
	}
	return collConf, nil
}

// LedgerInfoRetriever retrieves the relevant info from ledger
type LedgerInfoRetriever interface {
	GetBlockchainInfo() (*common.BlockchainInfo, error)
	NewQueryExecutor() (ledger.QueryExecutor, error)
}
