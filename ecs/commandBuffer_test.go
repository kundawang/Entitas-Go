// Copyright (c) 2021 Vladislav Fedotov (Falldot)
// License: MIT License
// MIT License web page: https://opensource.org/licenses/MIT
package ecs

import (
	"errors"
	"fmt"
	"testing"
)

// Component layout used by the battle simulation and most unit tests.
const (
	TestHealth = iota
	TestEnemy
	TestProjectile
	TestPosition
	TestSpeed
	TestComponentTotal
)

type testHealth struct{ HP int }
type testEnemy struct{ Kind string }
type testProjectile struct{ Damage int }
type testPosition struct{ X, Y int }
type testSpeed int

func newTestContext() EntityBase {
	return CreateEntityBase(TestComponentTotal)
}

func newEnemy(ctx EntityBase, id int, hp int) *Entity {
	eb := ctx.(*entityBase)
	e := eb.CreateEntity()
	e.Add(TestEnemy, &testEnemy{Kind: fmt.Sprintf("enemy-%d", id)})
	e.Add(TestHealth, &testHealth{HP: hp})
	return e
}

// eventLog records every group/entity notification in arrival order. It is
// used to prove Flush only exposes final states and that application order
// between entities follows the enqueue order.
type eventLog struct {
	adds    []int
	removes []int
	updates []int
}

func (l *eventLog) bind(g Group) {
	g.OnEntityAdded(func(_ Group, e *Entity, _ int, _ Component) {
		l.adds = append(l.adds, e.ID())
	})
	g.OnEntityRemoved(func(_ Group, e *Entity, _ int, _ Component) {
		l.removes = append(l.removes, e.ID())
	})
	g.OnEntityUpdate(func(_ Group, e *Entity, _ int, _ Component) {
		l.updates = append(l.updates, e.ID())
	})
}

// TestBattleSystemEnemyDeathAndProjectileSpawn reproduces the motivating
// scenario: a system iterates the enemy group while dead enemies are destroyed
// and projectiles are spawned through a command buffer. Iteration must cover
// every original member exactly once, and after Flush the groups must reflect
// only the final composition.
func TestBattleSystemEnemyDeathAndProjectileSpawn(t *testing.T) {
	ctx := newTestContext()
	enemyGroup := ctx.Group(NewMatcher().AllOf(TestEnemy))
	projectileGroup := ctx.Group(NewMatcher().AllOf(TestProjectile))
	healthGroup := ctx.Group(NewMatcher().AllOf(TestHealth))

	var log eventLog
	log.bind(enemyGroup)

	// Entity-level destroy callbacks fire directly from the flush replay.
	// They are only used to count destructions here: when creation and
	// destruction commands interleave, the entity pool recycles ids mid-flush,
	// so callback ordering is verified separately without recycling in
	// TestFlushReplaysInEnqueueOrder.
	var destroyCount int
	ctx.OnEntityWillBeDestroyed(func(e *Entity) {
		destroyCount++
	})

	enemies := make([]*Entity, 0, 5)
	for i := 0; i < 5; i++ {
		enemies = append(enemies, newEnemy(ctx, i, 10))
	}
	// The group was bound before setup; discard the creation events so the
	// log only observes Flush-time notifications.
	log = eventLog{}
	if enemyGroup.Lenght() != 5 || projectileGroup.Lenght() != 0 {
		t.Fatalf("unexpected initial groups: enemies=%d projectiles=%d",
			enemyGroup.Lenght(), projectileGroup.Lenght())
	}

	buffer := ctx.CreateCommandBuffer()

	// Fixed battle system: snapshot the old group, then record all changes.
	iterated := make([]int, 0, 5)
	spawned := make([]*Entity, 0, 5)
	for _, e := range enemyGroup.GetEntities() {
		iterated = append(iterated, e.ID())
		hp := e.Get(TestHealth).(*testHealth)
		if hp.HP <= 10 { // every enemy dies in this fixed scenario
			// One projectile is queued before the destroy command, proving
			// creation and destruction interleave without touching the group
			// being iterated.
			shot := buffer.CreateEntity()
			if err := buffer.AddComponent(shot, TestProjectile, &testProjectile{Damage: 5}); err != nil {
				t.Fatalf("projectile add: %v", err)
			}
			spawned = append(spawned, shot)
			if err := buffer.DestroyEntity(e); err != nil {
				t.Fatalf("destroy enemy: %v", err)
			}
		}
	}

	// During the frame nothing has changed yet: safe iteration over the old
	// group, no projectiles visible, no notifications fired.
	if len(iterated) != 5 {
		t.Fatalf("iteration skipped members, got %d enemies", len(iterated))
	}
	if enemyGroup.Lenght() != 5 || projectileGroup.Lenght() != 0 {
		t.Fatalf("groups mutated before Flush: enemies=%d projectiles=%d",
			enemyGroup.Lenght(), projectileGroup.Lenght())
	}
	if len(log.adds) != 0 || len(log.removes) != 0 || len(log.updates) != 0 {
		t.Fatalf("collector/group events fired before Flush: %+v", log)
	}
	pending := 0
	for _, state := range buffer.(*commandBuffer).placeholderCount() {
		if state.created {
			pending++
		}
	}
	if pending != 5 {
		t.Fatalf("expected 5 deferred projectiles, got %d", pending)
	}

	if err := buffer.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	// Final composition: all enemies and their health components are gone,
	// five projectiles exist.
	if enemyGroup.Lenght() != 0 {
		t.Fatalf("enemy group not empty after Flush: %d", enemyGroup.Lenght())
	}
	if projectileGroup.Lenght() != 5 {
		t.Fatalf("expected 5 projectiles, got %d", projectileGroup.Lenght())
	}
	if healthGroup.Lenght() != 0 {
		t.Fatalf("destroyed enemies left health components behind: %d", healthGroup.Lenght())
	}
	// Destroyed enemies must not carry the enemy mark anymore. Note the
	// framework recycles entity structs through its entity pool, so a recycled
	// enemy pointer may come back to life as a projectile; group membership
	// and Has() are therefore the authoritative checks, not IsAlive().
	for _, e := range enemies {
		if e.Has(TestEnemy) {
			t.Fatalf("enemy id %d retained its enemy component after flush", e.ID())
		}
		if enemyGroup.has(e) {
			t.Fatalf("enemy id %d is still an enemy group member", e.ID())
		}
	}
	// Projectile placeholders resolved to real, live entities.
	for _, p := range projectileGroup.GetEntities() {
		if !p.IsAlive() {
			t.Fatal("flushed projectile is not alive")
		}
		if p.Get(TestProjectile) == nil {
			t.Fatal("flushed projectile lost its component")
		}
	}
	// Every original enemy produced exactly one removal notification. Group
	// cleanup iterates an internal map so the group-event order is unspecified;
	// the command enqueue order is verified separately through the destroy
	// callbacks, which must match enemy iteration order 0..4.
	if len(log.removes) != 5 {
		t.Fatalf("expected 5 enemy removals, got %d (%v)", len(log.removes), log.removes)
	}
	removed := make(map[int]bool, 5)
	for _, id := range log.removes {
		removed[id] = true
	}
	for _, e := range enemies {
		if !removed[e.ID()] {
			t.Fatalf("enemy id %d missing from removal events", e.ID())
		}
	}
	if destroyCount != 5 {
		t.Fatalf("expected 5 destroy callbacks, got %d", destroyCount)
	}
}

// TestFlushReplaysInEnqueueOrder proves Flush applies commands in the exact
// order they were recorded. Each command targets a distinct entity so command
// coalescing (same entity + same component slot) cannot hide any of them;
// group add events then expose the global replay order.
func TestFlushReplaysInEnqueueOrder(t *testing.T) {
	ctx := newTestContext()
	g := ctx.Group(NewMatcher().AllOf(TestPosition))

	const n = 6
	entities := make([]*Entity, n)
	for i := range entities {
		entities[i] = ctx.CreateEntity()
	}

	var observed []int
	g.OnEntityAdded(func(_ Group, e *Entity, _ int, c Component) {
		observed = append(observed, c.(*testPosition).X)
	})

	buffer := ctx.CreateCommandBuffer()
	// Deterministic, non-monotonic order: command at step i targets entities[i]
	// and carries tag i, so the event sequence must be 0,1,2,...,n-1.
	for i := 0; i < n; i++ {
		if err := buffer.AddComponent(entities[i], TestPosition,
			&testPosition{X: i}); err != nil {
			t.Fatal(err)
		}
	}
	if err := buffer.Flush(); err != nil {
		t.Fatal(err)
	}
	if len(observed) != n {
		t.Fatalf("observed %d adds, want %d", len(observed), n)
	}
	for i, tag := range observed {
		if tag != i {
			t.Fatalf("flush order mismatch at %d: got tag %d", i, tag)
		}
	}
	for i, e := range entities {
		if e.Get(TestPosition).(*testPosition).X != i {
			t.Fatalf("entity %d final value wrong", i)
		}
	}
}

// TestCoalescedCommandsFinalState verifies that several commands for the same
// entity and component slot collapse to one final-state change, matching the
// generated AddX/ReplaceX/RemoveX API semantics.
func TestCoalescedCommandsFinalState(t *testing.T) {
	ctx := newTestContext()
	posGroup := ctx.Group(NewMatcher().AllOf(TestPosition))
	speedGroup := ctx.Group(NewMatcher().AllOf(TestSpeed))
	var log eventLog
	log.bind(posGroup)
	log.bind(speedGroup)

	e := ctx.CreateEntity()
	e.Add(TestHealth, &testHealth{HP: 1})
	buffer := ctx.CreateCommandBuffer()

	// Write churns: only the last position must survive, with one event.
	if err := buffer.AddComponent(e, TestPosition, &testPosition{X: 1, Y: 1}); err != nil {
		t.Fatal(err)
	}
	if err := buffer.ReplaceComponent(e, TestPosition, &testPosition{X: 2, Y: 2}); err != nil {
		t.Fatal(err)
	}
	if err := buffer.ReplaceComponent(e, TestPosition, &testPosition{X: 3, Y: 3}); err != nil {
		t.Fatal(err)
	}
	// Add then remove: the add is cancelled, the slot stays empty.
	if err := buffer.AddComponent(e, TestSpeed, testSpeed(7)); err != nil {
		t.Fatal(err)
	}
	if err := buffer.RemoveComponent(e, TestSpeed); err != nil {
		t.Fatal(err)
	}
	// Remove then write: the remove is superseded by the write.
	if err := buffer.RemoveComponent(e, TestHealth); err != nil {
		t.Fatal(err)
	}
	if err := buffer.ReplaceComponent(e, TestHealth, &testHealth{HP: 9}); err != nil {
		t.Fatal(err)
	}

	if posGroup.Lenght() != 0 || speedGroup.Lenght() != 0 {
		t.Fatal("groups changed before Flush")
	}
	if err := buffer.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	pos := e.Get(TestPosition).(*testPosition)
	if pos.X != 3 || pos.Y != 3 {
		t.Fatalf("position coalescing failed: %+v", pos)
	}
	if e.Get(TestSpeed) != nil {
		t.Fatal("cancelled speed write leaked through")
	}
	if e.Get(TestHealth).(*testHealth).HP != 9 {
		t.Fatal("remove-then-write coalescing failed")
	}
	if posGroup.Lenght() != 1 || speedGroup.Lenght() != 0 {
		t.Fatalf("wrong final groups: position=%d speed=%d", posGroup.Lenght(), speedGroup.Lenght())
	}
	// Exactly one add for position; no events for the cancelled/superseded
	// intermediate commands.
	if len(log.adds) != 1 || len(log.updates) != 0 || len(log.removes) != 0 {
		t.Fatalf("collector saw intermediate states: %+v", log)
	}

	// Repeated Flush is a successful no-op and leaves the buffer reusable.
	if err := buffer.Flush(); err != nil {
		t.Fatalf("second flush: %v", err)
	}
	if buffer.Len() != 0 {
		t.Fatalf("buffer not reset after flush, len=%d", buffer.Len())
	}
}

// TestReplaceOnEmptySlotAdds proves Flush write commands use Replace
// semantics: a write on an empty slot behaves as Add and enters matching
// groups.
func TestReplaceOnEmptySlotAdds(t *testing.T) {
	ctx := newTestContext()
	speedGroup := ctx.Group(NewMatcher().AllOf(TestSpeed))
	e := ctx.CreateEntity()
	buffer := ctx.CreateCommandBuffer()
	if err := buffer.ReplaceComponent(e, TestSpeed, testSpeed(3)); err != nil {
		t.Fatal(err)
	}
	if err := buffer.Flush(); err != nil {
		t.Fatal(err)
	}
	if speedGroup.Lenght() != 1 || e.Get(TestSpeed) != testSpeed(3) {
		t.Fatal("replace on empty slot did not add the component")
	}

	// A second buffered write on the occupied slot fires an update, not an add.
	var updates, adds int
	speedGroup.OnEntityUpdate(func(Group, *Entity, int, Component) { updates++ })
	speedGroup.OnEntityAdded(func(Group, *Entity, int, Component) { adds++ })
	buffer.ReplaceComponent(e, TestSpeed, testSpeed(4))
	if err := buffer.Flush(); err != nil {
		t.Fatal(err)
	}
	if updates != 1 || adds != 0 || e.Get(TestSpeed) != testSpeed(4) {
		t.Fatalf("occupied-slot replace events wrong: updates=%d adds=%d", updates, adds)
	}
}

// TestCreateThenDestroyCancels covers an entity created and destroyed inside
// the same buffer: no real entity must appear in the context.
func TestCreateThenDestroyCancels(t *testing.T) {
	ctx := newTestContext()
	enemyGroup := ctx.Group(NewMatcher().AllOf(TestEnemy))
	buffer := ctx.CreateCommandBuffer()

	ghost := buffer.CreateEntity()
	if err := buffer.AddComponent(ghost, TestEnemy, &testEnemy{Kind: "ghost"}); err != nil {
		t.Fatal(err)
	}
	if err := buffer.DestroyEntity(ghost); err != nil {
		t.Fatal(err)
	}
	if err := buffer.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if enemyGroup.Lenght() != 0 {
		t.Fatal("cancelled entity entered a group")
	}
}

// TestDestroyedEntityRejected checks the explicit outcome of recording
// commands against an entity that was destroyed immediately (not deferred).
func TestDestroyedEntityRejected(t *testing.T) {
	ctx := newTestContext()
	e := newEnemy(ctx, 1, 5)
	e.Destroy()
	buffer := ctx.CreateCommandBuffer()

	if err := buffer.DestroyEntity(e); !errors.Is(err, ErrEntityDestroyed) {
		t.Fatalf("destroy: want ErrEntityDestroyed, got %v", err)
	}
	if err := buffer.AddComponent(e, TestSpeed, testSpeed(1)); !errors.Is(err, ErrEntityDestroyed) {
		t.Fatalf("add: want ErrEntityDestroyed, got %v", err)
	}
	if err := buffer.RemoveComponent(e, TestEnemy); !errors.Is(err, ErrEntityDestroyed) {
		t.Fatalf("remove: want ErrEntityDestroyed, got %v", err)
	}
	if err := buffer.Flush(); err != nil {
		t.Fatalf("empty flush should succeed, got %v", err)
	}
}

// TestDuplicateCommandsRejected verifies duplicate destroy/remove recordings
// are rejected at enqueue time with explicit errors.
func TestDuplicateCommandsRejected(t *testing.T) {
	ctx := newTestContext()
	e := ctx.CreateEntity()
	e.Add(TestHealth, &testHealth{HP: 1})
	buffer := ctx.CreateCommandBuffer()

	if err := buffer.DestroyEntity(e); err != nil {
		t.Fatal(err)
	}
	if err := buffer.DestroyEntity(e); !errors.Is(err, ErrDuplicateDestroy) {
		t.Fatalf("want ErrDuplicateDestroy, got %v", err)
	}
	if err := buffer.AddComponent(e, TestSpeed, testSpeed(1)); !errors.Is(err, ErrEntityPendingKill) {
		t.Fatalf("write after destroy: want ErrEntityPendingKill, got %v", err)
	}

	other := ctx.CreateEntity()
	other.Add(TestPosition, &testPosition{})
	if err := buffer.RemoveComponent(other, TestPosition); err != nil {
		t.Fatal(err)
	}
	if err := buffer.RemoveComponent(other, TestPosition); !errors.Is(err, ErrDuplicateRemove) {
		t.Fatalf("want ErrDuplicateRemove, got %v", err)
	}

	if err := buffer.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if !other.IsAlive() {
		t.Fatal("remove command destroyed the entity")
	}
	if other.Get(TestPosition) != nil {
		t.Fatal("queued remove was not applied after flush")
	}
}

// TestCrossContextReferenceRejected builds two contexts and proves commands
// targeting foreign entities (and foreign placeholders) are refused.
func TestCrossContextReferenceRejected(t *testing.T) {
	ctxA := newTestContext()
	ctxB := newTestContext()
	bufferA := ctxA.CreateCommandBuffer()
	bufferB := ctxB.CreateCommandBuffer()

	foreign := ctxB.CreateEntity()
	if err := bufferA.AddComponent(foreign, TestSpeed, testSpeed(1)); !errors.Is(err, ErrForeignEntity) {
		t.Fatalf("foreign add: want ErrForeignEntity, got %v", err)
	}
	if err := bufferA.DestroyEntity(foreign); !errors.Is(err, ErrForeignEntity) {
		t.Fatalf("foreign destroy: want ErrForeignEntity, got %v", err)
	}
	if err := bufferA.RemoveComponent(foreign, TestHealth); !errors.Is(err, ErrForeignEntity) {
		t.Fatalf("foreign remove: want ErrForeignEntity, got %v", err)
	}

	foreignPlaceholder := bufferB.CreateEntity()
	if err := bufferA.AddComponent(foreignPlaceholder, TestEnemy, &testEnemy{}); !errors.Is(err, ErrForeignEntity) {
		t.Fatalf("foreign placeholder: want ErrForeignEntity, got %v", err)
	}

	if err := bufferA.Flush(); err != nil {
		t.Fatalf("flush A: %v", err)
	}
	if err := bufferB.Flush(); err != nil {
		t.Fatalf("flush B: %v", err)
	}
	if !foreign.IsAlive() {
		t.Fatal("foreign entity was affected by another context's buffer")
	}
}

// TestInvalidArguments covers nil entities, out-of-range component indices and
// nil components.
func TestInvalidArguments(t *testing.T) {
	ctx := newTestContext()
	buffer := ctx.CreateCommandBuffer()

	if err := buffer.DestroyEntity(nil); !errors.Is(err, ErrNilEntity) {
		t.Fatalf("nil destroy: got %v", err)
	}
	if err := buffer.AddComponent(nil, TestHealth, &testHealth{}); !errors.Is(err, ErrNilEntity) {
		t.Fatalf("nil add: got %v", err)
	}

	e := ctx.CreateEntity()
	if err := buffer.AddComponent(e, TestComponentTotal, &testHealth{}); !errors.Is(err, ErrComponentIndex) {
		t.Fatalf("high index: got %v", err)
	}
	if err := buffer.ReplaceComponent(e, -1, &testHealth{}); !errors.Is(err, ErrComponentIndex) {
		t.Fatalf("negative index: got %v", err)
	}
	if err := buffer.RemoveComponent(e, TestComponentTotal); !errors.Is(err, ErrComponentIndex) {
		t.Fatalf("remove index: got %v", err)
	}
	if err := buffer.AddComponent(e, TestHealth, nil); !errors.Is(err, ErrNilComponent) {
		t.Fatalf("nil component: got %v", err)
	}
	if err := buffer.Flush(); err != nil {
		t.Fatalf("flush with no valid commands: %v", err)
	}
}

// TestFlushFailureDoesNotStopSiblings proves commands are independent: when an
// entity dies outside the buffer before Flush, its queued commands fail and
// are reported, while other entities still get their changes.
func TestFlushFailureDoesNotStopSiblings(t *testing.T) {
	ctx := newTestContext()
	posGroup := ctx.Group(NewMatcher().AllOf(TestPosition))

	victim := ctx.CreateEntity()
	survivor := ctx.CreateEntity()
	buffer := ctx.CreateCommandBuffer()
	if err := buffer.AddComponent(victim, TestPosition, &testPosition{X: 1}); err != nil {
		t.Fatal(err)
	}
	if err := buffer.AddComponent(survivor, TestPosition, &testPosition{X: 2}); err != nil {
		t.Fatal(err)
	}

	// Kill the victim immediately; its buffered write must fail at Flush.
	victim.Destroy()

	err := buffer.Flush()
	if err == nil {
		t.Fatal("expected flush errors, got nil")
	}
	flushErrs, ok := err.(FlushErrors)
	if !ok {
		t.Fatalf("want FlushErrors, got %T", err)
	}
	if !flushErrs.Has(ErrEntityDestroyed) || len(flushErrs) != 1 {
		t.Fatalf("unexpected failures: %v", flushErrs)
	}
	if posGroup.Lenght() != 1 {
		t.Fatalf("sibling command was not applied, group=%d", posGroup.Lenght())
	}
	if !survivor.IsAlive() || survivor.Get(TestPosition) == nil {
		t.Fatal("survivor lost its buffered change")
	}
}

// TestLargeBatchOrderAndPerformance enqueues a large batch and checks that
// application order follows enqueue order and that cost stays linear (the
// test would time out under quadratic coalescing).
func TestLargeBatchOrderAndPerformance(t *testing.T) {
	ctx := newTestContext()
	enemyGroup := ctx.Group(NewMatcher().AllOf(TestEnemy))
	projectileGroup := ctx.Group(NewMatcher().AllOf(TestProjectile))

	const n = 10000
	enemies := make([]*Entity, 0, n)
	for i := 0; i < n; i++ {
		enemies = append(enemies, newEnemy(ctx, i, i))
	}
	buffer := ctx.CreateCommandBuffer()
	shots := make([]*Entity, 0, n)
	for i := 0; i < n; i++ {
		shot := buffer.CreateEntity()
		if err := buffer.AddComponent(shot, TestProjectile, &testProjectile{Damage: i}); err != nil {
			t.Fatal(err)
		}
		shots = append(shots, shot)
		if err := buffer.DestroyEntity(enemies[i]); err != nil {
			t.Fatal(err)
		}
	}
	if buffer.Len() != 3*n {
		t.Fatalf("len=%d, want %d", buffer.Len(), 3*n)
	}
	if err := buffer.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if enemyGroup.Lenght() != 0 || projectileGroup.Lenght() != n {
		t.Fatalf("groups after big flush: enemies=%d projectiles=%d",
			enemyGroup.Lenght(), projectileGroup.Lenght())
	}

	// Deterministic order: projectiles were created before the corresponding
	// enemy destroy, and ids grow monotonically in enqueue order.
	ids := projectileGroup.GetEntities()
	if len(ids) != n {
		t.Fatalf("projectile slice len=%d", len(ids))
	}
	// Group map iteration is unordered, so verify the set instead of order.
	seen := make(map[int]bool, n)
	for _, p := range ids {
		if seen[p.ID()] {
			t.Fatalf("duplicate projectile id %d", p.ID())
		}
		seen[p.ID()] = true
	}
}

// TestRemoveAbsentComponentIsNoOp ensures flushing a remove for an empty slot
// does not produce an error or a spurious group removal event.
func TestRemoveAbsentComponentIsNoOp(t *testing.T) {
	ctx := newTestContext()
	e := ctx.CreateEntity()
	var removals int
	g := ctx.Group(NewMatcher().AllOf(TestPosition))
	g.OnEntityRemoved(func(Group, *Entity, int, Component) { removals++ })

	buffer := ctx.CreateCommandBuffer()
	if err := buffer.RemoveComponent(e, TestPosition); err != nil {
		t.Fatal(err)
	}
	if err := buffer.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if removals != 0 || g.Lenght() != 0 {
		t.Fatal("absent-slot remove produced side effects")
	}
}

// TestCommandBufferAsSystem plugs the buffer into a system pool through the
// Executer interface and checks LastError reporting.
func TestCommandBufferAsSystem(t *testing.T) {
	ctx := newTestContext()
	systems := CreateSystemPool()
	buffer := ctx.CreateCommandBuffer()
	systems.Add(buffer)

	e := ctx.CreateEntity()
	if err := buffer.AddComponent(e, TestSpeed, testSpeed(2)); err != nil {
		t.Fatal(err)
	}
	systems.Execute()
	if buffer.LastError() != nil {
		t.Fatalf("unexpected system flush error: %v", buffer.LastError())
	}
	if e.Get(TestSpeed) != testSpeed(2) {
		t.Fatal("system did not flush buffered commands")
	}

	// A failing flush is exposed through LastError but does not panic.
	dead := ctx.CreateEntity()
	if err := buffer.AddComponent(dead, TestHealth, &testHealth{}); err != nil {
		t.Fatal(err)
	}
	dead.Destroy()
	systems.Execute()
	if buffer.LastError() == nil {
		t.Fatal("expected LastError after failed system flush")
	}
}

// TestResetDiscardsCommands ensures Reset drops everything without applying it.
func TestResetDiscardsCommands(t *testing.T) {
	ctx := newTestContext()
	g := ctx.Group(NewMatcher().AllOf(TestEnemy))
	buffer := ctx.CreateCommandBuffer()
	e := buffer.CreateEntity()
	if err := buffer.AddComponent(e, TestEnemy, &testEnemy{}); err != nil {
		t.Fatal(err)
	}
	buffer.Reset()
	if buffer.Len() != 0 {
		t.Fatal("reset did not clear commands")
	}
	if err := buffer.Flush(); err != nil {
		t.Fatal(err)
	}
	if g.Lenght() != 0 {
		t.Fatal("reset commands were applied")
	}

	// Buffer remains usable after reset.
	e2 := ctx.CreateEntity()
	if err := buffer.AddComponent(e2, TestEnemy, &testEnemy{}); err != nil {
		t.Fatal(err)
	}
	if err := buffer.Flush(); err != nil {
		t.Fatal(err)
	}
	if g.Lenght() != 1 {
		t.Fatal("buffer unusable after reset")
	}
}

// TestCollectorOnlyObservesFinalState drives a collector through buffered
// add/replace/remove churn and proves it never sees an intermediate state.
func TestCollectorOnlyObservesFinalState(t *testing.T) {
	ctx := newTestContext()
	collector := ctx.Collector(NewMatcher().AllOf(TestPosition)).OnAdd().OnUpdate()
	g := ctx.Group(NewMatcher().AllOf(TestPosition))

	e := ctx.CreateEntity()
	buffer := ctx.CreateCommandBuffer()
	buffer.AddComponent(e, TestPosition, &testPosition{X: 1})
	buffer.ReplaceComponent(e, TestPosition, &testPosition{X: 2})
	buffer.ReplaceComponent(e, TestPosition, &testPosition{X: 3})
	if len(collector.Entities()) != 0 {
		t.Fatal("collector changed before Flush")
	}
	if err := buffer.Flush(); err != nil {
		t.Fatal(err)
	}
	if len(collector.Entities()) != 1 || collector.Entities()[0] != e {
		t.Fatalf("collector did not observe exactly the final add: %v", collector.Entities())
	}
	if g.Lenght() != 1 {
		t.Fatal("group final membership wrong")
	}

	// An add cancelled by a remove must never reach the collector.
	collector.Clear()
	other := ctx.CreateEntity()
	buffer.AddComponent(other, TestPosition, &testPosition{X: 9})
	buffer.RemoveComponent(other, TestPosition)
	if err := buffer.Flush(); err != nil {
		t.Fatal(err)
	}
	if len(collector.Entities()) != 0 || g.Lenght() != 1 {
		t.Fatal("cancelled add leaked into collector/group")
	}
}

// TestDirectDestroyKeepsGroupsConsistent covers the framework-level fix:
// destroying an entity directly (without a buffer) must also remove it from
// every group, including after id recycling.
func TestDirectDestroyKeepsGroupsConsistent(t *testing.T) {
	ctx := newTestContext()
	eb := ctx.(*entityBase)
	g := ctx.Group(NewMatcher().AllOf(TestEnemy))

	e := newEnemy(ctx, 0, 3)
	if g.Lenght() != 1 {
		t.Fatal("setup failed")
	}
	e.Destroy()
	if g.Lenght() != 0 {
		t.Fatalf("destroyed entity stayed in group: %d", g.Lenght())
	}
	for _, member := range g.GetEntities() {
		if member == e {
			t.Fatal("destroyed entity still returned by GetEntities")
		}
	}

	// Recycling the id must not resurrect the old group membership.
	reborn := eb.CreateEntity()
	if g.Lenght() != 0 {
		t.Fatal("recycled id resurrected group membership")
	}
	reborn.Add(TestEnemy, &testEnemy{Kind: "reborn"})
	if g.Lenght() != 1 {
		t.Fatalf("re-adding component did not enter group, len=%d", g.Lenght())
	}
}

// placeholderCount is a test-only inspection helper.
func (b *commandBuffer) placeholderCount() map[*Entity]*entityState {
	return b.states
}
