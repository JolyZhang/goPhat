package vr

import (
	"github.com/mgentili/goPhat/phatlog"
	"log"
)

type StartViewChangeArgs struct {
	View          uint
	ReplicaNumber uint
}

type StartViewArgs struct {
	View         uint
	Log          *phatlog.Log
	OpNumber     uint
	CommitNumber uint
}

func (r *Replica) logVcstate(state string) {
	log.Printf("Replica %d at state %s: VN:%d, SVs:%d, DVs:%d\n", r.Rstate.ReplicaNumber, state, r.Rstate.View, r.Vcstate.StartViews, r.Vcstate.DoViews)
}

func (r *Replica) replicaStateInfo() {
	log.Printf("Replica %d: ViewNumber:%d, OpNumber:%d, CommitNumber:%d\n", r.Rstate.ReplicaNumber, r.Rstate.View, r.Rstate.OpNumber, r.Rstate.CommitNumber)
}

func (r *Replica) resetVcstate() {
	r.Vcstate = ViewChangeState{}
}

//A replica notices that a viewchange is needed - starts off the messages
func (r *Replica) PrepareViewChange() {
	r.Rstate.Status = ViewChange
	r.Rstate.View++
	r.logVcstate("PrepareViewChange")

	args := StartViewChangeArgs{r.Rstate.View, r.Rstate.ReplicaNumber}

	go r.sendAndRecv(NREPLICAS, "RPCReplica.StartViewChange", args,
		func() interface{} { return nil },
		func(r interface{}) bool { return false })

}

//viewchange RPCs
func (t *RPCReplica) StartViewChange(args *StartViewChangeArgs, reply *int) error {
	r := t.R

	//This view is already ahead of the proposed one
	if r.Rstate.View > args.View {
		return nil
	}

	//already recieved a message from this replica
	if ((1 << args.ReplicaNumber) & r.Vcstate.StartViewReplies) != 0 {
		return nil
	}

	r.Vcstate.StartViewReplies |= 1 << args.ReplicaNumber
	r.Vcstate.StartViews++
	r.logVcstate("StartViewChange")

	//first time we have seen this viewchange message
	if r.Rstate.View < args.View {
		r.Vcstate.NormalView = r.Rstate.View //last known normal View
		r.Rstate.View = args.View
		r.Rstate.Status = ViewChange

		SVCargs := StartViewChangeArgs{r.Rstate.View, r.Rstate.ReplicaNumber}

		//send StartViewChange messages to all replicas
		// TODO: we may want to only do this if our own master lease times out
		// (and not necessarily if we get a StartViewChange from someone else)
		// otherwise, we can potentially ditch our master too early, violating
		// the lease contract (which implies that a new master can't be
		// elected until a majority of the old master's leases expire)
		go r.sendAndRecv(NREPLICAS, "RPCReplica.StartViewChange", SVCargs,
			func() interface{} { return nil },
			func(r interface{}) bool { return false })
	}

	if r.Vcstate.StartViews == F {

		DVCargs := DoViewChangeArgs{r.Rstate.View, r.Rstate.ReplicaNumber, r.Phatlog, r.Vcstate.NormalView, r.Rstate.OpNumber, r.Rstate.CommitNumber}

		// only send DoViewChange if we're not the new master (can't actually send a message to ourself)
		if !r.IsMaster() {
			r.logVcstate("Sending DoViewChange")
			log.Printf("Sending to: %d\n", r.Rstate.View%(NREPLICAS+1))
			r.SendSync(r.Rstate.View%(NREPLICAS+1), "RPCReplica.DoViewChange", DVCargs, nil)
		}
	}

	return nil
}

func (t *RPCReplica) DoViewChange(args *DoViewChangeArgs, reply *int) error {
	r := t.R

	//already recieved a message from this replica
	if ((1 << args.ReplicaNumber) & r.Vcstate.DoViewReplies) != 0 {
		return nil
	}

	r.Vcstate.DoViewReplies |= 1 << args.ReplicaNumber
	r.Vcstate.DoViews++
	r.Vcstate.DoViewChangeMsgs[args.ReplicaNumber] = *args
	r.logVcstate("DoViewChange")

	//We have recived enough DoViewChange messages
	if r.Vcstate.DoViews == F {
		r.logVcstate("PrepareStartView")

		//updates replica state based on replies
		r.calcMasterView()

		// TODO: we don't technically have a master lease at this point
		r.Rstate.Status = Normal
		r.replicaStateInfo()
		r.BecomeMaster()
		r.logVcstate("ViewChangeComplete!")

		//send the StartView messages to all replicas
		SVargs := StartViewArgs{r.Rstate.View, r.Phatlog, r.Rstate.OpNumber, r.Rstate.CommitNumber}
		go r.sendAndRecv(NREPLICAS, "RPCReplica.StartView", SVargs,
			func() interface{} { return nil },
			func(r interface{}) bool { return false })

	}
	return nil
}

func (t *RPCReplica) StartView(args *DoViewChangeArgs, reply *int) error {
	r := t.R
	r.logVcstate("StartView")

	r.Phatlog = args.Log
	r.Rstate.OpNumber = args.OpNumber
	r.Rstate.CommitNumber = args.CommitNumber
	r.Rstate.Status = Normal
	r.Rstate.ExtendLease()

	r.replicaStateInfo()
	r.resetVcstate()
	r.logVcstate("ViewChangeComplete!")

	return nil
}

func (r *Replica) calcMasterView() {
	r.Rstate.View = r.Vcstate.DoViewChangeMsgs[0].View

	var maxOp uint = 0
	var maxCommit uint = 0
	var maxIdx uint = 0
	var maxNormalView uint = 0
	var maxView uint = 0

	for i := 0; i < NREPLICAS+1; i++ {
		//this is inefficient, but need to check for case where
		//replica does not send message
		if r.Vcstate.DoViewChangeMsgs[i].View > maxView {
			maxView = r.Vcstate.DoViewChangeMsgs[i].View
		}

		if uint(i) != r.Rstate.ReplicaNumber {
			if r.Vcstate.DoViewChangeMsgs[i].NormalView > maxNormalView || (r.Vcstate.DoViewChangeMsgs[i].NormalView == maxNormalView && r.Vcstate.DoViewChangeMsgs[i].OpNumber > maxOp) {
				maxOp = r.Vcstate.DoViewChangeMsgs[i].OpNumber
				maxIdx = uint(i)
			}

			if r.Vcstate.DoViewChangeMsgs[i].CommitNumber > maxCommit {
				maxCommit = r.Vcstate.DoViewChangeMsgs[i].CommitNumber
			}
		}
	}

	r.Rstate.View = maxView
	r.Phatlog = r.Vcstate.DoViewChangeMsgs[maxIdx].Log
	r.Rstate.OpNumber = maxOp
	r.Rstate.CommitNumber = maxCommit
}
