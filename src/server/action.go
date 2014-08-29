package server

import (
	"github.com/jliang00/gometa/src/common"
	"github.com/jliang00/gometa/src/protocol"
	"fmt"
	repo "github.com/jliang00/gometa/src/repository"
)

////////////////////////////////////////////////////////////////////////////
// Type Declaration
/////////////////////////////////////////////////////////////////////////////

type ServerAction struct {
	repo    *repo.Repository
	log     *repo.CommitLog
	config  *repo.ServerConfig
	server  ServerCallback
	factory protocol.MsgFactory
}

////////////////////////////////////////////////////////////////////////////
// Public Function
/////////////////////////////////////////////////////////////////////////////

func NewServerAction(s *Server) *ServerAction {

	return &ServerAction{repo: s.repo,
		log:     s.log,
		server:  s,
		config:  s.srvConfig,
		factory: s.factory}
}

////////////////////////////////////////////////////////////////////////////
// Server Action for Environment 
/////////////////////////////////////////////////////////////////////////////

func (a *ServerAction) GetEnsembleSize() uint64 {
	return uint64(len(GetPeerUDPAddr())) + 1  // including myself 
}

////////////////////////////////////////////////////////////////////////////
// Server Action for Broadcast stage (normal execution)
/////////////////////////////////////////////////////////////////////////////

func (a *ServerAction) Commit(p protocol.ProposalMsg) error {

	// TODO: Make the whole func transactional
	if err := a.persistChange(common.OpCode(p.GetOpCode()), p.GetKey(), p.GetContent()); err != nil {
		return err
	}
	
	if err := a.config.SetLastCommittedTxid(common.Txnid(p.GetTxnid())); err != nil {
		return err
	}

	a.server.UpdateStateOnCommit(p)

	return nil
}

func (a *ServerAction) LogProposal(p protocol.ProposalMsg) error {

	err := a.appendCommitLog(common.Txnid(p.GetTxnid()), common.OpCode(p.GetOpCode()), p.GetKey(), p.GetContent())
	if err != nil {
		return err
	}
	
	a.server.UpdateStateOnNewProposal(p)

	return nil
}

func (a *ServerAction) GetFollowerId() string {
	return GetHostTCPAddr() 
}

////////////////////////////////////////////////////////////////////////////
// Server Action for retrieving repository state
/////////////////////////////////////////////////////////////////////////////

func (a *ServerAction) GetLastLoggedTxid() (common.Txnid, error) {
	val, err := a.config.GetLastLoggedTxnId()
	return common.Txnid(val), err
}

func (a *ServerAction) GetLastCommittedTxid() (common.Txnid, error) {
	val, err := a.config.GetLastCommittedTxnId()
	return common.Txnid(val), err
}

func (a *ServerAction) GetStatus() protocol.PeerStatus {
	return a.server.GetState().getStatus()
}

func (a *ServerAction) GetCurrentEpoch() (uint32, error) {
	return a.config.GetCurrentEpoch()
}

func (a *ServerAction) GetAcceptedEpoch() (uint32, error) {
	return a.config.GetAcceptedEpoch()
}

////////////////////////////////////////////////////////////////////////////
// Server Action for updating repository state
/////////////////////////////////////////////////////////////////////////////

func (a *ServerAction) NotifyNewAcceptedEpoch(epoch uint32) error {
	oldEpoch, _ := a.GetAcceptedEpoch()
	
	// update only if the new epoch is larger
	if oldEpoch < epoch {  
		err := a.config.SetAcceptedEpoch(epoch)
		if err != nil {
			return err
		}
	}
	
	return nil
}

func (a *ServerAction) NotifyNewCurrentEpoch(epoch uint32) error {
	oldEpoch, _ := a.GetCurrentEpoch()
	
	// update only if the new epoch is larger
	if oldEpoch < epoch {  
		err := a.config.SetCurrentEpoch(epoch)
		if err != nil {
			return err
		}
		a.server.UpdateWinningEpoch(epoch)
	}
	
	return nil
}

////////////////////////////////////////////////////////////////////////////
// Function for discovery phase
/////////////////////////////////////////////////////////////////////////////

func (a *ServerAction) GetCommitedEntries(txid1, txid2 common.Txnid) (<- chan protocol.LogEntryMsg, <- chan error, chan <- bool, error) {

	// Get an iterator thas has exclusive write access.  This means there will not be
	// new commit entry being written while iterating.
	iter, err := a.log.NewIterator(txid1, txid2)
	if err != nil {
		return nil, nil, nil, err
	}

	logChan := make(chan protocol.LogEntryMsg, 100)
	errChan := make(chan error, 10)
	killChan := make(chan bool, 1)

	go a.startLogStreamer(txid1, iter, logChan, errChan, killChan)

	return logChan, errChan, killChan, nil
}

func (a *ServerAction) startLogStreamer(startTxid common.Txnid,
	iter *repo.LogIterator,
	logChan chan protocol.LogEntryMsg,
	errChan chan error,
	killChan chan bool) {

	// Close the iterator upon termination
	defer iter.Close()

	// TODO : Need to lock the commitLog so there is no new commit while streaming
	
	txnid, op, key, body, err := iter.Next() 
	for err == nil  {
		// only stream entry with a txid greater than the given one.  The caller would already 
		// have the entry for startTxid. If the caller use the boostrap value for txnid (0),
		// then this will stream everything.
		if txnid > startTxid {
			msg := a.factory.CreateLogEntry(uint64(txnid), uint32(op), key, body)
			select {
				case logChan <- msg:
				case _ = <- killChan :
					break
			}
		}
		txnid, op, key, body, err = iter.Next()
	}

	// Nothing more to send.  The entries will be in the channel until the reciever consumes them. 
	close(logChan)
	close(errChan)
}

func (a *ServerAction) LogAndCommit(txid common.Txnid, op uint32, key string, content []byte, toCommit bool) error {

	// TODO: Make this transactional

	if err := a.appendCommitLog(txid, common.OpCode(op), key, content); err != nil {
		return err
	}
	
	if toCommit {
		if err := a.persistChange(common.OpCode(op), key, content); err != nil {
			return err
		}
		
		return a.config.SetLastCommittedTxid(txid)
	}
	
	return nil
}

////////////////////////////////////////////////////////////////////////////
// Private Function
/////////////////////////////////////////////////////////////////////////////

func (a *ServerAction) get(key string) ([]byte, error) {

	newKey := fmt.Sprintf("%s%s", common.PREFIX_DATA_PATH, key)
	return a.repo.Get(newKey)
}

func (a *ServerAction) persistChange(op common.OpCode, key string, content []byte) error {

	newKey := fmt.Sprintf("%s%s", common.PREFIX_DATA_PATH, key)
	
	if op == common.OPCODE_ADD {
		return a.repo.Set(newKey, content) 
	}
		
	if op == common.OPCODE_SET {
		return a.repo.Set(newKey, content) 
	}

	if op == common.OPCODE_DELETE {
		return a.repo.Delete(newKey)
	}

	return common.NewError(common.PROTOCOL_ERROR, fmt.Sprintf("ServerAction.persistChange() : Unknown op code %d", op))
}

func (a *ServerAction) appendCommitLog(txnid common.Txnid, opCode common.OpCode, key string, content []byte) error {

	// TODO: Make the whole func transactional
	if err := a.log.Log(txnid, opCode, key, content); err != nil {
		return err
	}
	
	if err := a.config.SetLastLoggedTxid(txnid); err != nil {
		return err
	}
	
	return nil
}
