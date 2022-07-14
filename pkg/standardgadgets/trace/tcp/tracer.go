// Copyright 2022 The Inspektor Gadget authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package standard

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kinvolk/inspektor-gadget/pkg/gadgets/trace/tcp/tracer"
	"github.com/kinvolk/inspektor-gadget/pkg/gadgets/trace/tcp/types"
	"github.com/kinvolk/inspektor-gadget/pkg/standardgadgets/trace"
	eventtypes "github.com/kinvolk/inspektor-gadget/pkg/types"
)

type Tracer struct {
	trace.StandardTracerBase

	eventCallback func(types.Event)
	node          string
}

func NewTracer(config *tracer.Config,
	eventCallback func(types.Event), node string) (*Tracer, error,
) {
	lineCallback := func(line string) {
		event := types.Event{}
		event.Type = eventtypes.NORMAL

		// "Hack" to avoid changing the BCC tool implementation
		line = strings.ReplaceAll(line, `"ip"`, `"ipversion"`)
		line = strings.ReplaceAll(line, `"type"`, `"operation"`)

		if err := json.Unmarshal([]byte(line), &event); err != nil {
			msg := fmt.Sprintf("failed to unmarshal event '%s': %s", line, err)
			eventCallback(types.Base(eventtypes.Warn(msg, node)))
			return
		}

		eventCallback(event)
	}

	baseTracer, err := trace.NewStandardTracer(lineCallback, config.MountnsMap,
		"/usr/share/bcc/tools/tcptracer",
		"--json", "--containersmap", "/sys/fs/bpf/gadget/containers")
	if err != nil {
		return nil, err
	}

	return &Tracer{
		StandardTracerBase: *baseTracer,
		eventCallback:      eventCallback,
		node:               node,
	}, nil
}

func (t *Tracer) Stop() {
	if err := t.StandardTracerBase.Stop(); err != nil {
		t.eventCallback(types.Base(eventtypes.Warn(err.Error(), t.node)))
	}
}
