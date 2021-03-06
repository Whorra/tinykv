package raftstore

import (
	"fmt"
	"github.com/pingcap-incubator/tinykv/kv/raftstore/message"
	"github.com/pingcap-incubator/tinykv/kv/raftstore/meta"
	"github.com/pingcap-incubator/tinykv/kv/raftstore/util"
	"github.com/pingcap-incubator/tinykv/kv/util/engine_util"
	pb "github.com/pingcap-incubator/tinykv/proto/pkg/eraftpb"
	"github.com/pingcap-incubator/tinykv/proto/pkg/raft_cmdpb"
)

type proposalHandler struct {
	*peerMsgHandler
	//committed entries that need to handle
	Entries []pb.Entry
}

func newProposalHandler(msgHandler *peerMsgHandler, committedEntries []pb.Entry) *proposalHandler {
	return &proposalHandler{
		peerMsgHandler: msgHandler,
		Entries: committedEntries,
	}
}

//handle the proposal, main function in handler
func (h *proposalHandler)HandleProposal() {
	entrySize := len(h.Entries)
	if entrySize == 0  {
		//fmt.Println("Noop")
		return
	}
	for _,entry := range h.Entries {
		msg := &raft_cmdpb.RaftCmdRequest{}
		if err := msg.Unmarshal(entry.Data);err != nil {
			panic(err)
		}
		switch {
		case h.stopped == true:
			//fmt.Println("Stop")
			return
		case msg.AdminRequest != nil:
			h.handleAdminRequest(msg, &entry)
		case len(msg.Requests) > 0:
			//fmt.Println("handle req")
			h.handleRequest(msg, &entry)
		case entry.EntryType == pb.EntryType_EntryConfChange:

		}
	}
	kvWB := new(engine_util.WriteBatch)
	h.peerStorage.applyState.AppliedIndex = h.Entries[len(h.Entries)-1].Index
	kvWB.SetMeta(meta.ApplyStateKey(h.regionId), h.peerStorage.applyState)
	kvWB.WriteToDB(h.peerStorage.Engines.Kv)
	return
}

func (h *proposalHandler) handleAdminRequest(msg *raft_cmdpb.RaftCmdRequest, entry *pb.Entry) {

}

func (h *proposalHandler) handleRequest(msg *raft_cmdpb.RaftCmdRequest, entry *pb.Entry) {
	reqs := msg.Requests

	if err := h.checkKeyInRequests(reqs);err != nil {
		if cb,ok := h.checkProposalCb(entry);ok == true {
			cb.Done(ErrResp(err))
		}
		//fmt.Println("request not")
		return
	}

	kvWb := new(engine_util.WriteBatch)
	cb,ok := h.checkProposalCb(entry)
	resp := newCmdResp()
	for _,req := range reqs {
		//write command should be persisted first
		//fmt.Printf("\ncmd %v", req.CmdType)
		switch req.CmdType {
		case raft_cmdpb.CmdType_Put:
			kvWb.SetCF(req.Put.Cf, req.Put.Key, req.Put.Value)
		case raft_cmdpb.CmdType_Delete:
			kvWb.DeleteCF(req.Delete.Cf, req.Delete.Key)
		}
		kvWb.WriteToDB(h.peerStorage.Engines.Kv)
		kvWb.Reset()
		if ok == true {
			switch req.CmdType {
			case raft_cmdpb.CmdType_Get:
				h.peerStorage.applyState.AppliedIndex = entry.Index
				kvWb.SetMeta(meta.ApplyStateKey(h.regionId), h.peerStorage.applyState)
				val,_ := engine_util.GetCF(h.peerStorage.Engines.Kv, req.Get.Cf, req.Get.Key)
				resp.Responses = append(resp.Responses, &raft_cmdpb.Response{
					CmdType: raft_cmdpb.CmdType_Get,
					Get: &raft_cmdpb.GetResponse{Value: val},
				})
			case raft_cmdpb.CmdType_Delete:
				resp.Responses = append(resp.Responses, &raft_cmdpb.Response{
				CmdType: raft_cmdpb.CmdType_Delete,
				Delete: &raft_cmdpb.DeleteResponse{},
				})
			case raft_cmdpb.CmdType_Put:
				resp.Responses = append(resp.Responses, &raft_cmdpb.Response{
					CmdType: raft_cmdpb.CmdType_Put,
					Put: &raft_cmdpb.PutResponse{},
				})
			case raft_cmdpb.CmdType_Snap:
				if msg.Header.RegionEpoch.Version != h.Region().RegionEpoch.Version {
					cb.Done(ErrResp(&util.ErrEpochNotMatch{}))
					return
				}
				h.peerStorage.applyState.AppliedIndex = entry.Index
				kvWb.SetMeta(meta.ApplyStateKey(h.regionId), h.peerStorage.applyState)
				kvWb.WriteToDB(h.peerStorage.Engines.Kv)
				cb.Txn = h.peerStorage.Engines.Kv.NewTransaction(false)
				resp.Responses = append(resp.Responses, &raft_cmdpb.Response{
					CmdType: raft_cmdpb.CmdType_Snap,
					Snap: &raft_cmdpb.SnapResponse{Region: h.Region()},
				})
			}
			kvWb.WriteToDB(h.peerStorage.Engines.Kv)
		}
	}
	if cb != nil {
		//fmt.Printf("cb %v", reqs[0].CmdType)
		cb.Done(resp)
	}

}

//check the proposal is valid or not, if valid return the callback
func (h *proposalHandler) checkProposalCb(entry *pb.Entry) (*message.Callback,bool) {
	for {
		if len(h.proposals) == 0 {
			fmt.Println("no proposal1")
			return nil,false
		}
		proposal := h.proposals[0]
		h.proposals = h.proposals[1:]

		if proposal.term > entry.Term {
			fmt.Println("no proposal2")
			return nil,false
		}

		if proposal.index == entry.Index && proposal.term == entry.Term{
			fmt.Println("ture proposal")
			return proposal.cb,true
		}
		fmt.Println("stale proposal")
		NotifyStaleReq(entry.Term, proposal.cb)
	}
}
