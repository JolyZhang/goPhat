package queuedisk

import "os"
import "io/ioutil"
import "encoding/gob"
import "bytes"

type QCommand struct {
	Command string
	Value   string
}

type QResponse struct {
	Reply interface{}
	Error string
}

type QCommandWithChannel struct {
	Cmd  *QCommand
	Done chan *QResponse
}

func QueueServer(input chan QCommandWithChannel) {
	// Set up the queue
	mq := MessageQueue{}

	//if backup exists restore, otherwise we are just starting
	if _, err := os.Stat(queue_file); os.IsNotExist(err) {
		mq.Init()
	} else {
		var r []byte
		r, err = ioutil.ReadFile(queue_file)
		rbuf := bytes.NewBuffer(r)
		decoder := gob.NewDecoder(rbuf)
		err = decoder.Decode(mq)
	}

	// Enter the command loop
	for {
		request := <-input
		req := request.Cmd
		resp := &QResponse{}
		switch req.Command {
		case "PUSH":
			mq.Push(req.Value)
		case "POP":
			v := mq.Pop()
			if v != nil {
				resp.Reply = v
			} else {
				resp.Error = "Nothing to pop"
			}
		case "DONE":
			mq.Done(req.Value)
		case "LEN":
			resp.Reply = mq.Len()
		case "LEN_IN_PROGRESS":
			resp.Reply = mq.LenInProgress()
		default:
			resp.Error = "Unknown command"
		}

		request.Done <- resp
	}
}
