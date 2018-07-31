// Copyright 2016-2018, Pulumi Corporation.
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

package backend

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/pulumi/pulumi/pkg/resource"
	"github.com/pulumi/pulumi/pkg/resource/deploy"
	"github.com/pulumi/pulumi/pkg/tokens"
	"github.com/pulumi/pulumi/pkg/version"
	"github.com/pulumi/pulumi/pkg/workspace"
)

type MockRegisterResourceEvent struct {
	deploy.SourceEvent
}

func (m MockRegisterResourceEvent) Goal() *resource.Goal               { return nil }
func (m MockRegisterResourceEvent) Done(result *deploy.RegisterResult) {}

type MockStackPersister struct {
	SavedSnapshots []*deploy.Snapshot
}

func (m *MockStackPersister) Save(snap *deploy.Snapshot) error {
	m.SavedSnapshots = append(m.SavedSnapshots, snap)
	return nil
}

func (m *MockStackPersister) LastSnap() *deploy.Snapshot {
	return m.SavedSnapshots[len(m.SavedSnapshots)-1]
}

func MockSetup(t *testing.T, baseSnap *deploy.Snapshot) (*SnapshotManager, *MockStackPersister) {
	err := baseSnap.VerifyIntegrity()
	if !assert.NoError(t, err) {
		t.FailNow()
	}

	sp := &MockStackPersister{}
	return NewSnapshotManager(sp, baseSnap), sp
}

func NewResource(name string, deps ...resource.URN) *resource.State {
	return &resource.State{
		Type:         tokens.Type("test"),
		URN:          resource.URN(name),
		Inputs:       make(resource.PropertyMap),
		Outputs:      make(resource.PropertyMap),
		Dependencies: deps,
	}
}

func NewSnapshot(resources []*resource.State) *deploy.Snapshot {
	return deploy.NewSnapshot(deploy.Manifest{
		Time:    time.Now(),
		Version: version.Version,
		Plugins: nil,
	}, resources)
}

func TestIdenticalSames(t *testing.T) {
	sameState := NewResource("a-unique-urn")
	snap := NewSnapshot([]*resource.State{
		sameState,
	})

	manager, sp := MockSetup(t, snap)

	// The engine generates a SameStep on sameState.
	engineGeneratedSame := NewResource(string(sameState.URN))
	same := deploy.NewSameStep(nil, nil, sameState, engineGeneratedSame)

	mutation, err := manager.BeginMutation(same)
	assert.NoError(t, err)

	// No mutation was made
	assert.Empty(t, sp.SavedSnapshots)

	err = mutation.End(same, true)
	assert.NoError(t, err)

	// Sames `do` cause a snapshot mutation as part of `End`.
	assert.NotEmpty(t, sp.SavedSnapshots)
	assert.NotEmpty(t, sp.SavedSnapshots[0].Resources)

	// Our same resource should be the first entry in the snapshot list.
	inSnapshot := sp.SavedSnapshots[0].Resources[0]
	assert.Equal(t, sameState.URN, inSnapshot.URN)
}

// This test challenges the naive approach of mutating resources
// that are the targets of Same steps in-place by changing the dependencies
// of two resources in the snapshot, which is perfectly legal in our system
// (and in fact is done by the `dependency_steps` integration test as well).
//
// The correctness of the `snap` function in snapshot.go is tested here.
func TestSamesWithDependencyChanges(t *testing.T) {
	resourceA := NewResource("a-unique-urn-resource-a")
	resourceB := NewResource("a-unique-urn-resource-b", resourceA.URN)

	// The setup: the snapshot contains two resources, A and B, where
	// B depends on A. We're going to begin a mutation in which B no longer
	// depends on A and appears first in program order.
	snap := NewSnapshot([]*resource.State{
		resourceA,
		resourceB,
	})

	manager, sp := MockSetup(t, snap)

	resourceBUpdated := NewResource(string(resourceB.URN))
	// note: no dependencies

	resourceAUpdated := NewResource(string(resourceA.URN), resourceBUpdated.URN)
	// note: now depends on B

	// The engine first generates a Same for b:
	bSame := deploy.NewSameStep(nil, nil, resourceB, resourceBUpdated)
	mutation, err := manager.BeginMutation(bSame)
	assert.NoError(t, err)
	err = mutation.End(bSame, true)
	assert.NoError(t, err)

	// The snapshot should now look like this:
	//   snapshot
	//    resources
	//     b
	//     a
	// where b does not depend on anything and neither does a.
	firstSnap := sp.SavedSnapshots[0]
	assert.Len(t, firstSnap.Resources, 2)
	assert.Equal(t, resourceB.URN, firstSnap.Resources[0].URN)
	assert.Len(t, firstSnap.Resources[0].Dependencies, 0)
	assert.Equal(t, resourceA.URN, firstSnap.Resources[1].URN)
	assert.Len(t, firstSnap.Resources[1].Dependencies, 0)

	// The engine then generates a Same for a:
	aSame := deploy.NewSameStep(nil, nil, resourceA, resourceAUpdated)
	mutation, err = manager.BeginMutation(aSame)
	assert.NoError(t, err)
	err = mutation.End(aSame, true)
	assert.NoError(t, err)

	// The snapshot should now look like this:
	//   snapshot
	//    resources
	//     b
	//     a
	// where b does not depend on anything and a depends on b.
	secondSnap := sp.SavedSnapshots[1]
	assert.Len(t, secondSnap.Resources, 2)
	assert.Equal(t, resourceB.URN, secondSnap.Resources[0].URN)
	assert.Len(t, secondSnap.Resources[0].Dependencies, 0)
	assert.Equal(t, resourceA.URN, secondSnap.Resources[1].URN)
	assert.Len(t, secondSnap.Resources[1].Dependencies, 1)
	assert.Equal(t, resourceB.URN, secondSnap.Resources[1].Dependencies[0])
}

// This test exercises the merge operation with a particularly vexing deployment
// state that was useful in shaking out bugs.
func TestVexingDeployment(t *testing.T) {
	// This is the dependency graph we are going for in the base snapshot:
	//
	//       +-+
	//  +--> |A|
	//  |    +-+
	//  |     ^
	//  |    +-+
	//  |    |B|
	//  |    +-+
	//  |     ^
	//  |    +-+
	//  +--+ |C| <---+
	//       +-+     |
	//        ^      |
	//       +-+     |
	//       |D|     |
	//       +-+     |
	//               |
	//       +-+     |
	//       |E| +---+
	//       +-+
	a := NewResource("a")
	b := NewResource("b", a.URN)
	c := NewResource("c", a.URN, b.URN)
	d := NewResource("d", c.URN)
	e := NewResource("e", c.URN)
	snap := NewSnapshot([]*resource.State{
		a,
		b,
		c,
		d,
		e,
	})

	manager, sp := MockSetup(t, snap)

	// This is the sequence of events that come out of the engine:
	//   B - Same, depends on nothing
	//   C - CreateReplacement, depends on B
	//   C - Replace
	//   D - Update, depends on new C

	// This produces the following dependency graph in the new snapshot:
	//        +-+
	//  +---> |B|
	//  |     +++
	//  |      ^
	//  |     +++
	//  |     |C| <----+
	//  |     +-+      |
	//  |              |
	//  |     +-+      |
	//  +---+ |C| +-------------> A (not in graph!)
	//        +-+      |
	//                 |
	//        +-+      |
	//        |D|  +---+
	//        +-+
	//
	// Conceptually, this is a plan that deletes A. However, we have not yet observed the
	// deletion of A, presumably because the engine can't know for sure that it's been deleted
	// until the eval source completes. Of note in this snapshot is that the replaced C is still in the graph,
	// because it has not yet been deleted, and its dependency A is not in the graph because it
	// has not been seen.
	//
	// Since axiomatically we assume that steps come in in a valid topological order of the dependency graph,
	// we can logically assume that A is going to be deleted. (If A were not being deleted, it must have been
	// the target of a Step that came before C, which depends on it.)
	applyStep := func(step deploy.Step) {
		mutation, err := manager.BeginMutation(step)
		if !assert.NoError(t, err) {
			t.FailNow()
		}

		err = mutation.End(step, true)
		if !assert.NoError(t, err) {
			t.FailNow()
		}
	}

	// b now depends on nothing
	bPrime := NewResource(string(b.URN))
	applyStep(deploy.NewSameStep(nil, MockRegisterResourceEvent{}, b, bPrime))

	// c now only depends on b
	cPrime := NewResource(string(c.URN), bPrime.URN)

	// mocking out the behavior of a provider indicating that this resource needs to be deleted
	createReplacement := deploy.NewCreateReplacementStep(nil, MockRegisterResourceEvent{}, c, cPrime, nil, true)
	replace := deploy.NewReplaceStep(nil, c, cPrime, nil, true)
	c.Delete = true

	applyStep(createReplacement)
	applyStep(replace)

	// cPrime now exists, c is now pending deletion
	// dPrime now depends on cPrime, which got replaced
	dPrime := NewResource(string(d.URN), cPrime.URN)
	applyStep(deploy.NewUpdateStep(nil, MockRegisterResourceEvent{}, d, dPrime, nil))

	lastSnap := sp.SavedSnapshots[len(sp.SavedSnapshots)-1]
	assert.Len(t, lastSnap.Resources, 6)
	res := lastSnap.Resources

	// Here's what the merged snapshot should look like:
	// B should be first, and it should depend on nothing
	assert.Equal(t, b.URN, res[0].URN)
	assert.Len(t, res[0].Dependencies, 0)

	// cPrime should be next, and it should depend on B
	assert.Equal(t, c.URN, res[1].URN)
	assert.Len(t, res[1].Dependencies, 1)
	assert.Equal(t, b.URN, res[1].Dependencies[0])

	// d should be next, and it should depend on cPrime
	assert.Equal(t, d.URN, res[2].URN)
	assert.Len(t, res[2].Dependencies, 1)
	assert.Equal(t, c.URN, res[2].Dependencies[0])

	// a should be next, and it should depend on nothing
	assert.Equal(t, a.URN, res[3].URN)
	assert.Len(t, res[3].Dependencies, 0)

	// c should be next, it should depend on A and B and should be pending deletion
	// this is a critical operation of snap and the crux of this test:
	// merge MUST put c after a in the snapshot, despite never having seen a in the current plan
	assert.Equal(t, c.URN, res[4].URN)
	assert.True(t, res[4].Delete)
	assert.Len(t, res[4].Dependencies, 2)
	assert.Contains(t, res[4].Dependencies, a.URN)
	assert.Contains(t, res[4].Dependencies, b.URN)

	// e should be last, it should depend on C and still be live
	assert.Equal(t, e.URN, res[5].URN)
	assert.Len(t, res[5].Dependencies, 1)
	assert.Equal(t, c.URN, res[5].Dependencies[0])
}

func TestDeletion(t *testing.T) {
	resourceA := NewResource("a")
	snap := NewSnapshot([]*resource.State{
		resourceA,
	})

	manager, sp := MockSetup(t, snap)
	step := deploy.NewDeleteStep(nil, resourceA)
	mutation, err := manager.BeginMutation(step)
	if !assert.NoError(t, err) {
		t.FailNow()
	}

	err = mutation.End(step, true)
	if !assert.NoError(t, err) {
		t.FailNow()
	}

	// the end mutation should mark the resource as "done".
	// snap should then not put resourceA in the merged snapshot, since it has been deleted.
	lastSnap := sp.SavedSnapshots[len(sp.SavedSnapshots)-1]
	assert.Len(t, lastSnap.Resources, 0)
}

func TestFailedDelete(t *testing.T) {
	resourceA := NewResource("a")
	snap := NewSnapshot([]*resource.State{
		resourceA,
	})

	manager, sp := MockSetup(t, snap)
	step := deploy.NewDeleteStep(nil, resourceA)
	mutation, err := manager.BeginMutation(step)
	if !assert.NoError(t, err) {
		t.FailNow()
	}

	err = mutation.End(step, false /* successful */)
	if !assert.NoError(t, err) {
		t.FailNow()
	}

	// since we marked the mutation as not successful, the snapshot should still contain
	// the resource we failed to delete.
	lastSnap := sp.SavedSnapshots[len(sp.SavedSnapshots)-1]
	assert.Len(t, lastSnap.Resources, 1)
	assert.Equal(t, resourceA.URN, lastSnap.Resources[0].URN)
}

func TestStateChangeOnCreate(t *testing.T) {
	resourceA := NewResource("a")
	resourceB := NewResource("b")
	snap := NewSnapshot(nil)
	manager, sp := MockSetup(t, snap)

	step := deploy.NewCreateStep(nil, MockRegisterResourceEvent{}, resourceA)
	mutation, err := manager.BeginMutation(step)
	if !assert.NoError(t, err) {
		t.FailNow()
	}

	// BeginMutation should set the "State" flag on "resourceA" to "creating"
	lastSnap := sp.LastSnap()
	assert.Len(t, lastSnap.Resources, 1)
	assert.Equal(t, resourceA.URN, lastSnap.Resources[0].URN)
	assert.Equal(t, resource.OperationStatusCreating, lastSnap.Resources[0].Status)

	err = mutation.End(step, true /* successful */)
	if !assert.NoError(t, err) {
		t.FailNow()
	}

	lastSnap = sp.LastSnap()
	assert.Len(t, lastSnap.Resources, 1)
	assert.Equal(t, resourceA.URN, lastSnap.Resources[0].URN)
	assert.Equal(t, resource.OperationStatusEmpty, lastSnap.Resources[0].Status)

	// Try again with resourceB, but this time we're going to fail the mutation.
	step = deploy.NewCreateStep(nil, MockRegisterResourceEvent{}, resourceB)
	mutation, err = manager.BeginMutation(step)
	if !assert.NoError(t, err) {
		t.FailNow()
	}
	lastSnap = sp.LastSnap()
	assert.Len(t, lastSnap.Resources, 2)
	assert.Equal(t, resourceB.URN, lastSnap.Resources[1].URN)
	assert.Equal(t, resource.OperationStatusCreating, lastSnap.Resources[1].Status)

	// This time, with a failed mutation, the snapshot manager removes the incomplete resourceB
	// from the snapshot.
	err = mutation.End(step, false /* successful */)
	if !assert.NoError(t, err) {
		t.FailNow()
	}
	lastSnap = sp.LastSnap()
	assert.Len(t, lastSnap.Resources, 1)
	assert.Equal(t, resourceA.URN, lastSnap.Resources[0].URN)
}

func TestStateChangeOnUpdate(t *testing.T) {
	resourceAOld := NewResource("a")
	resourceAOld.Inputs["state"] = resource.NewStringProperty("old")
	resourceANew := NewResource("a")
	resourceANew.Inputs["state"] = resource.NewStringProperty("new")
	resourceBOld := NewResource("b")
	resourceBOld.Inputs["state"] = resource.NewStringProperty("old")
	resourceBNew := NewResource("b")
	resourceBNew.Inputs["state"] = resource.NewStringProperty("new")

	snap := NewSnapshot([]*resource.State{
		resourceAOld,
		resourceBOld,
	})
	manager, sp := MockSetup(t, snap)

	step := deploy.NewUpdateStep(nil, MockRegisterResourceEvent{}, resourceAOld, resourceANew, nil)
	mutation, err := manager.BeginMutation(step)
	if !assert.NoError(t, err) {
		t.FailNow()
	}

	// BeginMutation should append "a" to the snapshot immediately before the existing "a", but with the
	// "updating" flag set and the inputs set to the new inputs.
	lastSnap := sp.LastSnap()
	assert.Len(t, lastSnap.Resources, 3)
	assert.Equal(t, resourceAOld.URN, lastSnap.Resources[1].URN)
	assert.Equal(t, resource.NewStringProperty("old"), lastSnap.Resources[1].Inputs["state"])
	assert.Equal(t, resourceAOld.URN, lastSnap.Resources[0].URN)
	assert.Equal(t, resource.OperationStatusUpdating, lastSnap.Resources[0].Status)
	assert.Equal(t, resource.NewStringProperty("new"), lastSnap.Resources[0].Inputs["state"])

	// Successfully ending the mutation replaces the existing "a" in the snapshot with the new, updated inputs.
	err = mutation.End(step, true /* successful */)
	if !assert.NoError(t, err) {
		t.FailNow()
	}

	lastSnap = sp.LastSnap()
	assert.Len(t, lastSnap.Resources, 2)
	assert.Equal(t, resourceAOld.URN, lastSnap.Resources[0].URN)
	assert.Equal(t, resource.OperationStatusEmpty, resource.OperationStatusEmpty)
	assert.Equal(t, resource.NewStringProperty("new"), lastSnap.Resources[0].Inputs["state"])

	// Next, do an update that fails halfway through.
	// Snap places "b" immediately after the completed "a" in the snapshot.
	step = deploy.NewUpdateStep(nil, MockRegisterResourceEvent{}, resourceBOld, resourceBNew, nil)
	mutation, err = manager.BeginMutation(step)
	if !assert.NoError(t, err) {
		t.FailNow()
	}

	lastSnap = sp.LastSnap()
	assert.Len(t, lastSnap.Resources, 3)
	assert.Equal(t, resourceBOld.URN, lastSnap.Resources[1].URN)
	assert.Equal(t, resource.OperationStatusUpdating, lastSnap.Resources[1].Status)
	assert.Equal(t, resource.NewStringProperty("new"), lastSnap.Resources[1].Inputs["state"])
	assert.Equal(t, resourceBOld.URN, lastSnap.Resources[2].URN)
	assert.Equal(t, resource.OperationStatusEmpty, lastSnap.Resources[2].Status)
	assert.Equal(t, resource.NewStringProperty("old"), lastSnap.Resources[2].Inputs["state"])

	// A failed snapshot mutation removes the new "updating" resource from the snapshot without replacing
	// the existing resource.
	err = mutation.End(step, false /* successful */)
	if !assert.NoError(t, err) {
		t.FailNow()
	}
	lastSnap = sp.LastSnap()
	assert.Len(t, lastSnap.Resources, 2)
	assert.Equal(t, resourceBOld.URN, lastSnap.Resources[1].URN)
	assert.Equal(t, resource.OperationStatusEmpty, lastSnap.Resources[1].Status)
	assert.Equal(t, resource.NewStringProperty("old"), lastSnap.Resources[1].Inputs["state"])
}

func TestStateChangeOnDelete(t *testing.T) {
	resourceA := NewResource("a")
	resourceB := NewResource("b")
	snap := NewSnapshot([]*resource.State{
		resourceA,
		resourceB,
	})
	manager, sp := MockSetup(t, snap)

	step := deploy.NewDeleteStep(nil, resourceA)
	mutation, err := manager.BeginMutation(step)
	if !assert.NoError(t, err) {
		t.FailNow()
	}

	// BeginMutation should mutate the *existing* resource to have status "deleting". It should not
	// insert any additional resources.
	lastSnap := sp.LastSnap()
	assert.Len(t, lastSnap.Resources, 2)
	assert.Equal(t, resourceA.URN, lastSnap.Resources[0].URN)
	assert.Equal(t, resource.OperationStatusDeleting, lastSnap.Resources[0].Status)

	err = mutation.End(step, true /* successful */)
	if !assert.NoError(t, err) {
		t.FailNow()
	}

	// When the deletion succeeds, the resource should be removed from the snapshot.
	lastSnap = sp.LastSnap()
	assert.Len(t, lastSnap.Resources, 1)
	assert.Equal(t, resourceB.URN, lastSnap.Resources[0].URN)

	stepB := deploy.NewDeleteStep(nil, resourceB)
	mutation, err = manager.BeginMutation(stepB)
	if !assert.NoError(t, err) {
		t.FailNow()
	}
	lastSnap = sp.LastSnap()
	assert.Len(t, lastSnap.Resources, 1)
	assert.Equal(t, resourceB.URN, lastSnap.Resources[0].URN)
	assert.Equal(t, resource.OperationStatusDeleting, lastSnap.Resources[0].Status)

	err = mutation.End(stepB, false /* successful */)
	if !assert.NoError(t, err) {
		t.FailNow()
	}
	lastSnap = sp.LastSnap()
	assert.Len(t, lastSnap.Resources, 1)
	assert.Equal(t, resourceB.URN, lastSnap.Resources[0].URN)
	assert.Equal(t, resource.OperationStatusEmpty, lastSnap.Resources[0].Status)
}

func TestStateChangeOnReadExistingResource(t *testing.T) {
	resourceA := NewResource("a")
	resourceA.External = true
	resourceA.Custom = true
	resourceANew := NewResource("a")
	resourceANew.External = true
	resourceANew.Custom = true
	resourceANew.Inputs["key"] = resource.NewStringProperty("read")

	resourceB := NewResource("b")
	resourceB.External = true
	resourceB.Custom = true
	resourceBNew := NewResource("b")
	resourceBNew.External = true
	resourceBNew.Custom = true
	resourceBNew.Inputs["key"] = resource.NewStringProperty("read")

	snap := NewSnapshot([]*resource.State{
		resourceA,
		resourceB,
	})
	manager, sp := MockSetup(t, snap)
	step := deploy.NewReadStep(nil, nil, resourceA, resourceANew)
	mutation, err := manager.BeginMutation(step)
	if !assert.NoError(t, err) {
		t.FailNow()
	}

	// BeginMutation should add resourceANew to the snapshot with the "state" field set to "reading". It should
	// be placed before the existing resource.
	lastSnap := sp.LastSnap()
	assert.Len(t, lastSnap.Resources, 3)
	assert.Equal(t, resourceA.URN, lastSnap.Resources[0].URN)
	assert.Equal(t, resource.OperationStatusReading, lastSnap.Resources[0].Status)
	assert.Equal(t, resource.NewStringProperty("read"), lastSnap.Resources[0].Inputs["key"])
	assert.Equal(t, resourceA.URN, lastSnap.Resources[1].URN)
	assert.Equal(t, resource.OperationStatusEmpty, lastSnap.Resources[1].Status)

	err = mutation.End(step, true /* successful */)
	if !assert.NoError(t, err) {
		t.FailNow()
	}

	// A successful mutation should remove the existing resource from the snapshot and clear the "reading" status
	// on the new resource.
	lastSnap = sp.LastSnap()
	assert.Len(t, lastSnap.Resources, 2)
	assert.Equal(t, resourceA.URN, lastSnap.Resources[0].URN)
	assert.Equal(t, resource.OperationStatusEmpty, lastSnap.Resources[0].Status)
	assert.Equal(t, resource.NewStringProperty("read"), lastSnap.Resources[0].Inputs["key"])

	// Next, do a Read on resourceB that fails.
	step = deploy.NewReadStep(nil, nil, resourceB, resourceBNew)
	mutation, err = manager.BeginMutation(step)
	if !assert.NoError(t, err) {
		t.FailNow()
	}

	err = mutation.End(step, false /* successful */)
	if !assert.NoError(t, err) {
		t.FailNow()
	}

	// The failed mutation should remove the new "reading" resource from the snapshot and leave the existing
	// resource intact.
	lastSnap = sp.LastSnap()
	assert.Len(t, lastSnap.Resources, 2)
	assert.Equal(t, resourceB.URN, lastSnap.Resources[1].URN)
	assert.Equal(t, resource.OperationStatusEmpty, lastSnap.Resources[1].Status)
	assert.NotEqual(t, resource.NewStringProperty("read"), lastSnap.Resources[1].Inputs["key"])
}

func TestStateChangeOnReadNewResource(t *testing.T) {
	resourceA := NewResource("a")
	resourceA.External = true
	resourceA.Custom = true
	resourceB := NewResource("a")
	resourceB.External = true
	resourceB.Custom = true

	snap := NewSnapshot(nil)
	manager, sp := MockSetup(t, snap)
	step := deploy.NewReadStep(nil, nil, nil, resourceA)
	mutation, err := manager.BeginMutation(step)
	if !assert.NoError(t, err) {
		t.FailNow()
	}

	// BeginMutation should add resourceA to the snapshot with the "state" field set to "reading".
	lastSnap := sp.LastSnap()
	assert.Len(t, lastSnap.Resources, 1)
	assert.Equal(t, resourceA.URN, lastSnap.Resources[0].URN)
	assert.Equal(t, resource.OperationStatusReading, lastSnap.Resources[0].Status)

	err = mutation.End(step, true /* successful */)
	if !assert.NoError(t, err) {
		t.FailNow()
	}

	// A successful mutation should clear the "reading" status on the new resource.
	lastSnap = sp.LastSnap()
	assert.Len(t, lastSnap.Resources, 1)
	assert.Equal(t, resourceA.URN, lastSnap.Resources[0].URN)
	assert.Equal(t, resource.OperationStatusEmpty, lastSnap.Resources[0].Status)

	// Next, do a Read on resourceB that fails.
	step = deploy.NewReadStep(nil, nil, nil, resourceB)
	mutation, err = manager.BeginMutation(step)
	if !assert.NoError(t, err) {
		t.FailNow()
	}

	err = mutation.End(step, false /* successful */)
	if !assert.NoError(t, err) {
		t.FailNow()
	}

	// A failed mutation should remove the new "reading" resource from the snapshot.
	lastSnap = sp.LastSnap()
	assert.Len(t, lastSnap.Resources, 1)
	assert.Equal(t, resourceA.URN, lastSnap.Resources[0].URN)
	assert.Equal(t, resource.OperationStatusEmpty, lastSnap.Resources[0].Status)
}

func TestRegisterOutputs(t *testing.T) {
	resourceA := NewResource("a")
	snap := NewSnapshot([]*resource.State{
		resourceA,
	})
	manager, sp := MockSetup(t, snap)

	// There should be zero snaps performed at the start.
	assert.Len(t, sp.SavedSnapshots, 0)

	// The step here is not important.
	step := deploy.NewSameStep(nil, nil, resourceA, resourceA)
	err := manager.RegisterResourceOutputs(step)
	if !assert.NoError(t, err) {
		t.FailNow()
	}

	// The RegisterResourceOutputs should have caused a snapshot to be written.
	assert.Len(t, sp.SavedSnapshots, 1)

	// It should be identical to what has already been written.
	lastSnap := sp.LastSnap()
	assert.Len(t, lastSnap.Resources, 1)
	assert.Equal(t, resourceA.URN, lastSnap.Resources[0].URN)
	assert.Equal(t, resource.OperationStatusEmpty, lastSnap.Resources[0].Status)
}

func TestSavePlugins(t *testing.T) {
	snap := NewSnapshot(nil)
	manager, sp := MockSetup(t, snap)

	err := manager.RecordPlugin(workspace.PluginInfo{
		Name: "myplugin",
	})
	if !assert.NoError(t, err) {
		t.FailNow()
	}

	// RecordPlugin should have written out a new snapshot with the plugin recorded in the manifest.
	lastSnap := sp.LastSnap()
	assert.Len(t, lastSnap.Manifest.Plugins, 1)
	assert.Equal(t, "myplugin", lastSnap.Manifest.Plugins[0].Name)
}
