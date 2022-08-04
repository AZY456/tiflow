// Copyright 2022 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package runtime

import (
	frameModel "github.com/pingcap/tiflow/engine/framework/model"
	"github.com/pingcap/tiflow/engine/jobmaster/dm/metadata"
)

// TaskStatus defines the running task status.
type TaskStatus struct {
	Unit           frameModel.WorkerType
	Task           string
	Stage          metadata.TaskStage
	CfgModRevision uint64
}

// NewOfflineStatus is used when jobmaster receives a worker offline.
func NewOfflineStatus(task string) TaskStatus {
	return TaskStatus{Task: task, Stage: metadata.StageUnscheduled}
}
