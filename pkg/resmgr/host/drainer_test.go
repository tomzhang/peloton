// Copyright (c) 2019 Uber Technologies, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package host

import (
	"fmt"
	"testing"
	"time"

	"github.com/uber/peloton/.gen/peloton/api/v0/peloton"
	"github.com/uber/peloton/.gen/peloton/private/hostmgr/hostsvc"
	host_mocks "github.com/uber/peloton/.gen/peloton/private/hostmgr/hostsvc/mocks"
	"github.com/uber/peloton/.gen/peloton/private/resmgr"
	res_mocks "github.com/uber/peloton/pkg/resmgr/respool/mocks"

	"github.com/uber/peloton/pkg/common"
	"github.com/uber/peloton/pkg/common/eventstream"
	"github.com/uber/peloton/pkg/common/lifecycle"
	"github.com/uber/peloton/pkg/common/stringset"
	preemption_mocks "github.com/uber/peloton/pkg/resmgr/preemption/mocks"
	rm_task "github.com/uber/peloton/pkg/resmgr/task"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/suite"
	"github.com/uber-go/tally"
)

const (
	hostname      = "testHostname"
	drainerPeriod = 1 * time.Second
	taskName      = "testTask"
)

type DrainerTestSuite struct {
	suite.Suite
	mockCtrl           *gomock.Controller
	tracker            rm_task.Tracker
	drainer            Drainer
	preemptor          *preemption_mocks.MockQueue
	mockHostmgr        *host_mocks.MockInternalHostServiceYARPCClient
	eventStreamHandler *eventstream.Handler
	hostnames          []string
}

func (suite *DrainerTestSuite) SetupSuite() {
	suite.mockCtrl = gomock.NewController(suite.T())
	rm_task.InitTaskTracker(tally.NoopScope, &rm_task.Config{})
	suite.tracker = rm_task.GetTracker()
}

func (suite *DrainerTestSuite) SetupTest() {
	suite.mockHostmgr = host_mocks.NewMockInternalHostServiceYARPCClient(suite.mockCtrl)

	suite.eventStreamHandler = eventstream.NewEventStreamHandler(
		1000,
		[]string{
			common.PelotonJobManager,
			common.PelotonResourceManager,
		},
		nil,
		tally.Scope(tally.NoopScope))

	suite.preemptor = preemption_mocks.NewMockQueue(suite.mockCtrl)

	suite.drainer = Drainer{
		drainerPeriod:   drainerPeriod,
		hostMgrClient:   suite.mockHostmgr,
		preemptionQueue: suite.preemptor,
		rmTracker:       suite.tracker,
		lifecycle:       lifecycle.NewLifeCycle(),
		drainingHosts:   stringset.New(),
	}

	t := &resmgr.Task{
		Name:     taskName,
		JobId:    &peloton.JobID{Value: "job1"},
		Id:       &peloton.TaskID{Value: taskName},
		Hostname: hostname,
	}

	suite.addTaskToTracker(t)
	suite.hostnames = []string{hostname}
}

func (suite *DrainerTestSuite) addTaskToTracker(t *resmgr.Task) {
	mockRespool := res_mocks.NewMockResPool(suite.mockCtrl)
	mockRespool.EXPECT().GetPath().Return("mockRespoolPath")
	suite.tracker.AddTask(
		t, suite.eventStreamHandler,
		mockRespool,
		&rm_task.Config{})
}

func TestDrainer(t *testing.T) {
	suite.Run(t, new(DrainerTestSuite))
}

func (suite *DrainerTestSuite) TearDownTest() {
	suite.tracker.Clear()
}

func (suite *DrainerTestSuite) TestDrainer_Init() {
	r := NewDrainer(
		tally.NoopScope,
		suite.mockHostmgr,
		drainerPeriod,
		suite.tracker,
		suite.preemptor)
	suite.NotNil(r)
}

func (suite *DrainerTestSuite) TestDrainer_StartStop() {
	defer func() {
		suite.drainer.Stop()
		_, ok := <-suite.drainer.lifecycle.StopCh()
		suite.False(ok)

		// Stopping drainer again should be no-op
		err := suite.drainer.Stop()
		suite.NoError(err)
	}()
	err := suite.drainer.Start()
	suite.NoError(err)
	suite.NotNil(suite.drainer.lifecycle.StopCh())

	// Starting drainer again should be no-op
	err = suite.drainer.Start()
	suite.NoError(err)
}

func (suite *DrainerTestSuite) TestDrainCycle_EnqueueError() {
	suite.mockHostmgr.EXPECT().
		GetDrainingHosts(gomock.Any(), gomock.Any()).
		Return(&hostsvc.GetDrainingHostsResponse{
			Hostnames: suite.hostnames,
		}, nil)
	suite.preemptor.EXPECT().
		EnqueueTasks(gomock.Any(), gomock.Any()).
		Return(fmt.Errorf("fake Enqueue error"))
	err := suite.drainer.performDrainCycle()
	suite.Error(err)
	suite.drainer.drainingHosts.Clear()
}

func (suite *DrainerTestSuite) TestDrainCycle() {
	suite.tracker.Clear()

	suite.preemptor.EXPECT().
		EnqueueTasks(gomock.Any(), gomock.Any()).
		Return(nil).Times(2)

	// simulate 2 cycles
	for i := 0; i < 2; i++ {
		// add tasks to tracker
		hostname := fmt.Sprintf("hostname-%d", i)
		suite.addTaskToTracker(&resmgr.Task{
			Name:     taskName,
			JobId:    &peloton.JobID{Value: "job1"},
			Id:       &peloton.TaskID{Value: taskName},
			Hostname: hostname,
		})
		hosts := []string{hostname}

		suite.mockHostmgr.EXPECT().
			GetDrainingHosts(gomock.Any(), gomock.Any()).
			Return(&hostsvc.GetDrainingHostsResponse{
				Hostnames: hosts,
			}, nil).Times(1)

		err := suite.drainer.performDrainCycle()
		suite.NoError(err)

		// the drainer should only have the newest host.
		suite.Len(suite.drainer.drainingHosts.ToSlice(), len(suite.hostnames))
		for _, host := range hosts {
			suite.Equal(true, suite.drainer.drainingHosts.Contains(host))
		}
	}
}
func (suite *DrainerTestSuite) TestDrainCycle_NoHostsToDrain() {
	suite.mockHostmgr.EXPECT().
		GetDrainingHosts(gomock.Any(), gomock.Any()).
		Return(&hostsvc.GetDrainingHostsResponse{}, nil)
	err := suite.drainer.performDrainCycle()
	suite.NoError(err)
}

func (suite *DrainerTestSuite) TestDrainCycle_GetDrainingHostsError() {
	suite.mockHostmgr.EXPECT().
		GetDrainingHosts(gomock.Any(), gomock.Any()).
		Return(nil, fmt.Errorf("fake GetDrainingHosts error"))
	err := suite.drainer.performDrainCycle()
	suite.Error(err)
}

func (suite *DrainerTestSuite) TestDrainCycle_MarkHostsDrained() {
	suite.mockHostmgr.EXPECT().
		GetDrainingHosts(gomock.Any(), gomock.Any()).
		Return(&hostsvc.GetDrainingHostsResponse{
			Hostnames: []string{"dummyhost"},
		}, nil)
	suite.mockHostmgr.EXPECT().
		MarkHostsDrained(gomock.Any(), gomock.Any()).
		Return(&hostsvc.MarkHostsDrainedResponse{}, nil)
	err := suite.drainer.performDrainCycle()
	suite.NoError(err)
}
