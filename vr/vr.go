package vr

import (
	"errors"
	"fmt"
	"github.com/mgentili/goPhat/phatlog"
	"log"
	"net"
	"net/rpc"
	"runtime"
	"time"
)

const (
	F           = 1
	NREPLICAS   = 2 * F // doesn't count the master as a replica
	LEASE       = 2000 * time.Millisecond
    // how soon master renews lease before actual expiry date. e.g. if lease expires in 100 seconds
    // the master starts trying to renew the lease after 100/RENEW_FACTOR seconds
    RENEW_FACTOR = 2
    // the margin we allow different replicas' clocks to be off by and still have correct behavior
    MAX_CLOCK_DRIFT = LEASE / 10
	// don't resend requests too much, as it will just end up flooding
	// crashed nodes when they come back online
	MAX_TRIES = 2
	// doubles after every failure
	BACKOFF_TIME = 10 * time.Millisecond
)

// a replica's possible states
const (
	Normal = iota
	Recovery
	ViewChange
)

type Command string

type Replica struct {
	Rstate   ReplicaState
	Mstate   MasterState
	Vcstate  ViewChangeState
	Rcvstate RecoveryState
	// list of replica addresses, in sorted order
	Config  []string
	Clients [NREPLICAS + 1]*rpc.Client
	Phatlog *phatlog.Log
	// function to call to commit to a command
	CommitFunc func(command interface{})
	Listener   net.Listener
	IsShutdown bool
}

/* special object just for RPC calls, so that other methods
 * can take a Replica object and not be considered RPCs
 */
type RPCReplica struct {
	R *Replica
}

type ReplicaState struct {
	View           uint
	OpNumber       uint
	CommitNumber   uint
	ReplicaNumber  uint
	Status         int
	NormalView     uint
	ViewChangeMsgs uint
	Timer          *time.Timer
}

type MasterState struct {
	A int
	// bit vector of what replicas have replied
	Replies uint64

	Timer            *time.Timer
	Heartbeats       map[uint]time.Time
	HeartbeatReplies uint64
}

type ViewChangeState struct {
	DoViewChangeMsgs [NREPLICAS + 1]DoViewChangeArgs
	DoViewReplies    uint64
	StartViewReplies uint64
	StartViews       uint
	DoViews          uint
	NormalView       uint
}

type RecoveryState struct {
	RecoveryResponseMsgs    [NREPLICAS + 1]RecoveryResponseArgs
	RecoveryResponseReplies uint64
	RecoveryResponses       uint
	Nonce                   uint
}

type DoViewChangeArgs struct {
	View          uint
	ReplicaNumber uint
	Log           *phatlog.Log
	NormalView    uint
	OpNumber      uint
	CommitNumber  uint
}

type RecoveryArgs struct {
	ReplicaNumber uint
	Nonce         uint
}

type RecoveryResponseArgs struct {
	View          uint
	Nonce         uint
	Log           *phatlog.Log
	OpNumber      uint
	CommitNumber  uint
	ReplicaNumber uint
}

type PrepareArgs struct {
	View         uint
	Command      interface{}
	OpNumber     uint
	CommitNumber uint
}

type PrepareReply struct {
	View          uint
	OpNumber      uint
	ReplicaNumber uint
    Lease time.Time
}

type CommitArgs struct {
	View         uint
	CommitNumber uint
}

type HeartbeatReply struct {
    ReplicaNumber uint
    Lease time.Time
}

// Go doesn't have assertions...
func assert(b bool) {
	if !b {
		_, file, line, _ := runtime.Caller(1)
		log.Fatalf("assertion failed: %s:%d", file, line)
	}
}

func wrongView() error {
	return errors.New("view numbers don't match")
}

func (r *Replica) addLog(command interface{}) {
	r.Phatlog.Add(r.Rstate.OpNumber, command)
	r.Debug("adding command to log")
}

func (r *Replica) Debug(format string, args ...interface{}) {
	str := fmt.Sprintf("Replica %d: %s", r.Rstate.ReplicaNumber, format)
	log.Printf(str, args...)
}

func (r *Replica) doCommit(cn uint) {
	if cn <= r.Rstate.CommitNumber {
		r.Debug("Ignoring commit %d, already commited up to %d", cn, r.Rstate.CommitNumber)
		return
	} else if cn > r.Rstate.OpNumber {
		r.Debug("need to do state transfer. only at op %d in log but got commit for %d\n", r.Rstate.OpNumber, cn)
		r.PrepareRecovery()
		return
	} else if cn > r.Rstate.CommitNumber+1 {
		r.Debug("need to do extra commits")
		// we're behind (but have all the log entries, so don't need to state
		// transfer), so catch up by committing up to the current commit
		for i := r.Rstate.CommitNumber + 1; i < cn; i++ {
			r.doCommit(i)
		}
	}
	r.Debug("commiting %d", r.Rstate.CommitNumber+1)
	if r.CommitFunc != nil {
		r.CommitFunc(r.Phatlog.GetCommand(r.Rstate.CommitNumber + 1))
	}
	r.Rstate.CommitNumber++
}

// RPCs
func (t *RPCReplica) Prepare(args *PrepareArgs, reply *PrepareReply) error {
	r := t.R
	r.Debug("Got prepare %d\n", args.OpNumber)

	if args.View > r.Rstate.View {
		// a new master must have been elected without us, so need to recover
		r.PrepareRecovery()
		//TODO: should we return an error, block until recovery completes, or
		// something else??
		return errors.New("recovering")
	} else if args.View < r.Rstate.View {
		// message from the old master, ignore
		return wrongView()
	}

	if r.Rstate.Status != Normal {
		// TODO: ideally we should just not respond or something in this case?
		return errors.New("not in normal mode")
	}

	if args.OpNumber <= r.Rstate.OpNumber {
		// master must be resending some old request?
		return errors.New("old op number")
	}
	if args.OpNumber > r.Rstate.OpNumber+1 {
		// we must be behind?
		r.PrepareRecovery()
		return fmt.Errorf("op numbers out of sync: got %d expected %d", args.OpNumber, r.Rstate.OpNumber+1)
	}

    reply.Lease = time.Now().Add(LEASE)
    reply.ReplicaNumber = r.Rstate.ReplicaNumber
	r.Rstate.ExtendLease(reply.Lease)

	r.Rstate.OpNumber++
	r.addLog(args.Command)

	// commit the last thing if necessary (this reduces the number of actual
	// commit messages that need to be sent)
	r.doCommit(args.CommitNumber)

	reply.View = r.Rstate.View
	reply.OpNumber = r.Rstate.OpNumber

	return nil
}

func (t *RPCReplica) Commit(args *CommitArgs, reply *HeartbeatReply) error {
	r := t.R

	if args.View > r.Rstate.View {
		// a new master must have been elected without us, so need to recover
		r.PrepareRecovery()
		return errors.New("doing a recovery")
	} else if args.View < r.Rstate.View {
		// message from the old master, ignore
		return wrongView()
	}

    reply.ReplicaNumber = r.Rstate.ReplicaNumber
    reply.Lease = time.Now().Add(LEASE)
	r.Rstate.ExtendLease(reply.Lease)

	r.doCommit(args.CommitNumber)

	return nil
}

func (r *Replica) IsMaster() bool {
	return r.Rstate.View%(NREPLICAS+1) == r.Rstate.ReplicaNumber
}

func (mstate *MasterState) Reset() {
	mstate.A = 0
	mstate.Replies = 0
}

func (mstate *MasterState) ExtendNeedsRenewal(newTime time.Time) {
	mstate.Timer.Reset((newTime - time.Now()) / RENEW_FACTOR + time.Now())
}

func (r *Replica) Shutdown() {
	r.Listener.Close()
	r.Rstate.Timer.Stop()
	r.Mstate.Timer.Stop()
	r.Mstate.Reset()
	r.IsShutdown = true
}

// closes connection to the given replica number
func (r *Replica) DestroyConns(repNum uint) {
	if r.Clients[repNum] != nil {
		r.Clients[repNum].Close()
	}
}

func (a []time.Time) Len() int { return len(a) }
func (a []time.Time) Swap(i, j int) { a[i], a[j] = a[j], a[i] }
func (a []time.Time) Less(i, j int) bool { return a[i].Before(a[j])  }

func SortTimes(times map[uint]time.Time) []time.Time {
    vals := make([]time.Time, len(times))
    i := 0
    for _, v := range m {
        vals[i] = k
        i++
    }
    sort.Sort(vals)
}

func (r *Replica) Heartbeat(replica uint, newTime time.Time) {
	assert(r.IsMaster())

	r.Mstate.Heartbeats[replica] = newTime

    sortedTimes := SortTimes(r.Mstate.Heartbeats)
    assert(len(sortedTimes) == NREPLICAS)
    leaseExpiry := sortedTimes[F].Add(-MAX_CLOCK_DRIFT)
    r.Mstate.ExtendNeedsRenewal(leaseExpiry)
    r.Rstate.ExtendLease(leaseExpiry)
}

func (rstate *ReplicaState) ExtendLease(newTime time.Time) {
	rstate.Timer.Reset(time.Sub(newTime, time.Now()))
}

func (r *Replica) ReplicaTimeout() {
	if r.IsMaster() {
		r.Debug("we couldn't stay master :(,ViewNum:%d\n", r.Rstate.View)
		// TODO: can't handle read requests anymore
	}
	r.Debug("Timed out, trying view change")
	r.PrepareViewChange()
	// start counting again so we timeout if the new replica can't become master
	r.Rstate.ExtendLease(time.Now().Add(LEASE))
}

func (r *Replica) MasterNeedsRenewal() {
	if r.IsShutdown {
		return
	}
	r.sendCommitMsgs()
}

func (r *Replica) sendCommitMsgs() {
	args := CommitArgs{r.Rstate.View, r.Rstate.CommitNumber}
	r.Debug("sending commit: %d", r.Rstate.CommitNumber)
	go r.sendAndRecv(NREPLICAS, "RPCReplica.Commit", args,
		func() interface{} { return new(uint) },
		func(reply interface{}) bool { r.Heartbeat(*(reply.(*uint))); return false })
}

func RunAsReplica(i uint, config []string) *Replica {
	r := new(Replica)
	r.Rstate.ReplicaNumber = i
	r.Config = config

	r.ReplicaInit()

	go r.ReplicaRun()
	if r.IsMaster() {
		r.BecomeMaster()
	}

	return r
}

func (r *Replica) BecomeMaster() {
	assert(r.IsMaster())
	// TODO: anything else we need to do to become the master?
	r.Mstate.Reset()
	// resets master's timer
    // TODO: this should be done differently
	r.Mstate.ExtendNeedsRenewal(time.Now().Add(LEASE - MAX_CLOCK_DRIFT))
	r.Rstate.ExtendLease(time.Now().Add(LEASE - MAX_CLOCK_DRIFT))
}

func (r *Replica) ReplicaInit() {
	ln, err := net.Listen("tcp", r.Config[r.Rstate.ReplicaNumber])
	if err != nil {
		r.Debug("Couldn't start a listener: %v", err)
		return
	}
	r.Listener = ln
	r.Rstate.Timer = time.AfterFunc(LEASE, r.ReplicaTimeout)
	// set up master time even as a replica, so that if we do become master
	// the timer object already exists
	r.Mstate.Timer = time.AfterFunc(MAX_RENEWAL, r.MasterNeedsRenewal)
	r.Mstate.Timer.Stop()
	r.Phatlog = phatlog.EmptyLog()
}

func (r *Replica) ReplicaRun() {
	newServer := rpc.NewServer()

	rpcreplica := new(RPCReplica)
	rpcreplica.R = r
	newServer.Register(rpcreplica)

	for {
		conn, err := r.Listener.Accept()
		if err != nil {
			r.Debug("err: %v", err)
			time.Sleep(10000 * time.Millisecond)
			continue
		}
		go newServer.ServeConn(conn)
	}
}

func (r *Replica) RunVR(command interface{}) {
	assert(r.IsMaster() /*&& holdLease()*/)

	// FIXME: right now we enforce that the last operation has been committed before starting a new one
	assert(r.Rstate.OpNumber == r.Rstate.CommitNumber)

	r.Mstate.Reset()

	r.Rstate.OpNumber++
	r.addLog(command)

	args := PrepareArgs{r.Rstate.View, command, r.Rstate.OpNumber, r.Rstate.CommitNumber}
	replyConstructor := func() interface{} { return new(PrepareReply) }
	r.sendAndRecv(NREPLICAS, "RPCReplica.Prepare", args, replyConstructor, func(reply interface{}) bool {
		return r.handlePrepareOK(reply.(*PrepareReply))
	})
}

func (r *Replica) handlePrepareOK(reply *PrepareReply) bool {
	r.Debug("got response: %+v\n", reply)
	if reply.View != r.Rstate.View {
		return false
	}

	r.Heartbeat(reply.ReplicaNumber)

	if reply.OpNumber != r.Rstate.OpNumber {
		return false
	}

	if ((1 << reply.ReplicaNumber) & r.Mstate.Replies) != 0 {
		return false
	}

	r.Debug("got suitable response\n")

	r.Mstate.Replies |= 1 << reply.ReplicaNumber
	r.Mstate.A++

	r.Debug("new master state: %v\n", r.Mstate)

	// we've implicitly gotten a response from ourself already
	if r.Mstate.A != F {
		return r.Mstate.A >= F
	}

	// we've now gotten a majority
	r.doCommit(r.Rstate.CommitNumber + 1)

	// TODO: we shouldn't really need to do this (only on periods of inactivity)
	r.sendCommitMsgs()

	return true
}

func (r *Replica) SendSync(repNum uint, msg string, args interface{}, reply interface{}) {
	r.sendAndRecvTo([]uint{repNum}, msg, args, func() interface{} { return reply }, func(r interface{}) bool { return false })
}

func (r *Replica) ClientConnect(repNum uint) error {
	assert(repNum != r.Rstate.ReplicaNumber)
	c, err := rpc.Dial("tcp", r.Config[repNum])

	if err != nil {
		r.Debug("error trying to connect to replica %d: %v", repNum, err)
	} else {
		if r.Clients[repNum] != nil {
			r.Clients[repNum].Close()
		}
		r.Clients[repNum] = c
	}

	return err
}

// same as sendAndRecvTo but just picks any N replicas
func (r *Replica) sendAndRecv(N int, msg string, args interface{}, newReply func() interface{}, handler func(reply interface{}) bool) {
	assert(N <= NREPLICAS)
	reps := make([]uint, N)
	i := 0
	for repNum := uint(0); i < N && repNum < NREPLICAS+1; repNum++ {
		if repNum == r.Rstate.ReplicaNumber {
			continue
		}
		reps[i] = repNum
		i++
	}
	r.sendAndRecvTo(reps, msg, args, newReply, handler)
}

/* Sends RPC to the given list of replicas
* msg is the RPC call name
* args is the argument struct
* newReply is a constructor that returns a new object of the expected reply
  type. This is a bit of a wart of Go, because you can't really pass types
  to a function, but we still need a way to keep making new reply objects
* handler is a function that will be called and passed the resulting reply
  for each reply that is received. It will be called until it returns true,
  which signals that enough replies have been received that sendAndRecv
  will return (e.g. a majority has been received).
* Note, however, that the RPCs will generally be re-sent until N responses
* are received, even when handler returns true. This is so all replicas
* do eventually get the message, even once a majority has been reached
* and other operations can continue
*/
//TODO: need to handle the case where handler never returns true e.g.
// because we were in a network partition and couldn't reach any other
// replicas. eventually we should exit but still somehow signify failure
func (r *Replica) sendAndRecvTo(replicas []uint, msg string, args interface{}, newReply func() interface{}, handler func(reply interface{}) bool) {
	type ReplicaCall struct {
		Reply interface{}
		Error error
		// need to track which client so we can resend as needed
		RepNum uint
		Tries  uint
	}

	callChan := make(chan ReplicaCall)

	// blocks til completion
	sendOne := func(repNum uint, tries uint) {
		var call ReplicaCall
		call.RepNum = repNum
		call.Tries = tries + 1

		// might need to first open a connection to them
		if r.Clients[repNum] == nil {
			call.Error = r.ClientConnect(repNum)
			if call.Error != nil {
				callChan <- call
				return
			}
		}
		client := r.Clients[repNum]
		call.Reply = newReply()
		call.Error = client.Call(msg, args, call.Reply)
		// and now send it to the master channel
		callChan <- call
	}

	// send requests to the replicas
	for _, repNum := range replicas {
		if repNum == r.Rstate.ReplicaNumber {
			continue
		}
		go sendOne(repNum, 0)
	}

	doneChan := make(chan int)

	go func() {
		callHandler := true
		// and now get the responses and retry if necessary
		N := len(replicas)
		for i := 0; i < N; {
			call := <-callChan
			if call.Error != nil {
				// for now just resend failed messages indefinitely
				r.Debug("sendAndRecv message error: %v", call.Error)
				if call.Error == rpc.ErrShutdown {
					// connection is shutdown so force reconnect
					r.Clients[call.RepNum].Close()
					r.Clients[call.RepNum] = nil
				}
				// give up eventually (mainly, helps recovery errors actually show up)
				if call.Tries >= MAX_TRIES {
					//i++
					continue
				}
				go func() {
					// exponential backoff
					time.Sleep(BACKOFF_TIME * (1 << (call.Tries - 1)))
					sendOne(call.RepNum, call.Tries)
				}()
				continue
			}
			if callHandler && handler(call.Reply) {
				// signals doneChan so that sendAndRecv can exit
				// (and the master can continue to the next request)
				// we still continue and resend messages as neccesary, however
				doneChan <- 0
				callHandler = false
			}

			i++
		}
		// handler never returned true, but we've sent all the messages we needed to, so can fully exit
		if callHandler {
			doneChan <- 0
		}
	}()

	<-doneChan
}
