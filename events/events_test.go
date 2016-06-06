/*
Copyright IBM Corp. 2016 All Rights Reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

		 http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package events

import (
	"fmt"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/hyperledger/fabric/events/consumer"
	"github.com/hyperledger/fabric/events/producer"
	ehpb "github.com/hyperledger/fabric/protos"
	"github.com/spf13/viper"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/grpclog"
)

type Adapter struct {
	sync.RWMutex
	notfy chan struct{}
	count int
}

var peerAddress string
var adapter *Adapter
var obcEHClient *consumer.EventsClient

func (a *Adapter) GetInterestedEvents() ([]*ehpb.Interest, error) {
	return []*ehpb.Interest{
		&ehpb.Interest{EventType: ehpb.EventType_BLOCK},
		&ehpb.Interest{EventType: ehpb.EventType_CHAINCODE, RegInfo: &ehpb.Interest_ChaincodeRegInfo{&ehpb.ChaincodeReg{ChaincodeID: "0xffffffff", EventName: "event1"}}},
		&ehpb.Interest{EventType: ehpb.EventType_CHAINCODE, RegInfo: &ehpb.Interest_ChaincodeRegInfo{&ehpb.ChaincodeReg{ChaincodeID: "0xffffffff", EventName: ""}}},
	}, nil
	//return []*ehpb.Interest{&ehpb.Interest{EventType: ehpb.EventType_BLOCK}}, nil
}

func (a *Adapter) Recv(msg *ehpb.Event) (bool, error) {
	//fmt.Printf("Adapter received %+v\n", msg.Event)
	switch x := msg.Event.(type) {
	case *ehpb.Event_Block:
	case *ehpb.Event_Generic:
	case *ehpb.Event_ChaincodeEvent:
	case nil:
		// The field is not set.
		fmt.Printf("event not set\n")
		return false, fmt.Errorf("event not set")
	default:
		fmt.Printf("unexpected type %T\n", x)
		return false, fmt.Errorf("unexpected type %T", x)
	}
	a.Lock()
	a.count--
	if a.count <= 0 {
		a.notfy <- struct{}{}
	}
	a.Unlock()
	return true, nil
}

func (a *Adapter) Disconnected(err error) {
	if err != nil {
		fmt.Printf("Error: %s\n", err)
	}
}

func createTestBlock() *ehpb.Event {
	emsg := producer.CreateBlockEvent(&ehpb.Block{Transactions: []*ehpb.Transaction{}})
	return emsg
}

func createTestChaincodeEvent(tid string, typ string) *ehpb.Event {
	emsg := producer.CreateChaincodeEvent(&ehpb.ChaincodeEvent{ChaincodeID: tid, EventName: typ})
	return emsg
}

func createTestGenericEvent() *ehpb.Event {
	emsg := producer.CreateGenericEvent(&ehpb.Generic{EventType: "uuid#ccEventName", Payload: []byte("event data")})
	return emsg
}

func closeListenerAndSleep(l net.Listener) {
	l.Close()
	time.Sleep(2 * time.Second)
}

// Test the invocation of a transaction.
func TestReceiveMessage(t *testing.T) {
	var err error

	adapter.count = 1
	//emsg := createTestBlock()
	emsg := createTestChaincodeEvent("0xffffffff", "event1")
	if err = producer.Send(emsg); err != nil {
		t.Fail()
		t.Logf("Error sending message %s", err)
	}

	//receive 2 messages
	for i := 0; i < 2; i++ {
		select {
		case <-adapter.notfy:
		case <-time.After(5 * time.Second):
			t.Fail()
			t.Logf("timed out on messge")
		}
	}
}

func TestReceiveAnyMessage(t *testing.T) {
	var err error

	adapter.count = 1
	emsg := createTestBlock()
	if err = producer.Send(emsg); err != nil {
		t.Fail()
		t.Logf("Error sending message %s", err)
	}

	emsg = createTestChaincodeEvent("0xffffffff", "event2")
	if err = producer.Send(emsg); err != nil {
		t.Fail()
		t.Logf("Error sending message %s", err)
	}

	//receive 2 messages - a block and a chaincode event
	for i := 0; i < 2; i++ {
		select {
		case <-adapter.notfy:
		case <-time.After(5 * time.Second):
			t.Fail()
			t.Logf("timed out on messge")
		}
	}
}

func TestFailReceive(t *testing.T) {
	var err error

	adapter.count = 1
	emsg := createTestChaincodeEvent("badcc", "event1")
	if err = producer.Send(emsg); err != nil {
		t.Fail()
		t.Logf("Error sending message %s", err)
	}

	select {
	case <-adapter.notfy:
		t.Fail()
		t.Logf("should NOT have received event1")
	case <-time.After(2 * time.Second):
	}
}

func BenchmarkMessages(b *testing.B) {
	numMessages := 10000

	adapter.count = numMessages

	var err error
	//b.ResetTimer()

	for i := 0; i < numMessages; i++ {
		go func() {
			//emsg := createTestGenericEvent()
			//emsg := createTestBlock()
			emsg := createTestChaincodeEvent("0xffffffff", "event1")
			if err = producer.Send(emsg); err != nil {
				b.Fail()
				b.Logf("Error sending message %s", err)
			}
		}()
	}

	select {
	case <-adapter.notfy:
	case <-time.After(5 * time.Second):
		b.Fail()
		b.Logf("timed out on messge")
	}
}

func TestMain(m *testing.M) {
	SetupTestConfig()
	var opts []grpc.ServerOption
	if viper.GetBool("peer.tls.enabled") {
		creds, err := credentials.NewServerTLSFromFile(viper.GetString("peer.tls.cert.file"), viper.GetString("peer.tls.key.file"))
		if err != nil {
			grpclog.Fatalf("Failed to generate credentials %v", err)
		}
		opts = []grpc.ServerOption{grpc.Creds(creds)}
	}
	grpcServer := grpc.NewServer(opts...)

	//use a different address than what we usually use for "peer"
	//we override the peerAddress set in chaincode_support.go
	peerAddress = "0.0.0.0:60303"

	lis, err := net.Listen("tcp", peerAddress)
	if err != nil {
		fmt.Printf("Error starting events listener %s....not doing tests", err)
		return
	}

	// Register EventHub server
	// use a buffer of 100 and blocking timeout
	ehServer := producer.NewEventsServer(100, 0)
	ehpb.RegisterEventsServer(grpcServer, ehServer)

	fmt.Printf("Starting events server\n")
	go grpcServer.Serve(lis)

	done := make(chan struct{})
	adapter = &Adapter{notfy: done}
	obcEHClient = consumer.NewEventsClient(peerAddress, adapter)
	if err = obcEHClient.Start(); err != nil {
		fmt.Printf("could not start chat %s\n", err)
		obcEHClient.Stop()
		return
	}

	time.Sleep(2 * time.Second)

	os.Exit(m.Run())
}
