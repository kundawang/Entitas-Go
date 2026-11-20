// Copyright (c) 2021 Vladislav Fedotov (Falldot)
// License: MIT License
// MIT License web page: https://opensource.org/licenses/MIT
package ecs

import "errors"

// Command kinds recorded by a CommandBuffer. They are applied in the exact
// order in which they were enqueued when Flush is called.
const (
	commandCreate = iota
	commandDestroy
	commandWrite // Add/Replace share one kind: the final state only carries
	// the last written component, exactly like Entity.Replace.
	commandRemove
)

// Errors returned synchronously while recording, or wrapped inside the
// FlushErrors result of Flush when a command can no longer be applied.
var (
	ErrNilEntity         = errors.New("command buffer: entity is nil")
	ErrForeignEntity     = errors.New("command buffer: entity belongs to another context")
	ErrEntityDestroyed   = errors.New("command buffer: entity has already been destroyed")
	ErrEntityPendingKill = errors.New("command buffer: entity already has a destroy command")
	ErrComponentIndex    = errors.New("command buffer: component index out of range")
	ErrNilComponent      = errors.New("command buffer: component is nil")
	ErrDuplicateDestroy  = errors.New("command buffer: destroy command already queued for entity")
	ErrDuplicateRemove   = errors.New("command buffer: remove command already queued for component")
)

// CommandBuffer defers entity and component changes until Flush. Systems can
// iterate Groups and Collectors while recording creation, destruction and
// component add/remove/replace commands; nothing they record mutates a Group
// or fires a Collector event before Flush. Commands of one entity are
// coalesced so that after Flush the groups and collectors only observe the
// entity's final state, while the visible application order between entities
// stays the enqueue order.
type CommandBuffer interface {
	// CreateEntity returns a detached placeholder entity. Component commands
	// issued on it are buffered; the real entity is created in the context at
	// Flush time and receives the same components.
	CreateEntity() *Entity

	// DestroyEntity defers entity destruction.
	DestroyEntity(e *Entity) error

	// AddComponent defers adding a component. If a write for the same slot is
	// already queued it is replaced, matching the generated AddX/ReplaceX API
	// (only the last value survives).
	AddComponent(e *Entity, index int, component Component) error

	// ReplaceComponent defers replacing a component. At Flush the command
	// behaves like Entity.Replace: it adds when the slot is empty, replaces
	// otherwise.
	ReplaceComponent(e *Entity, index int, component Component) error

	// RemoveComponent defers removing a component. A queued write for the same
	// slot is cancelled.
	RemoveComponent(e *Entity, index int) error

	// Flush applies every recorded command in enqueue order, then resets the
	// buffer so it can be reused. It returns nil on success or a FlushErrors
	// value describing every command that could not be applied.
	Flush() error

	// Reset discards all recorded commands without applying them.
	Reset()

	// Len returns the number of recorded commands, including commands that
	// were superseded by later commands of the same entity.
	Len() int

	// Executer makes the buffer usable as a system: each Execute call flushes
	// pending commands. The last flush error (if any) is retained.
	Executer()

	// LastError returns the error produced by the latest Executer flush.
	LastError() error
}

type command struct {
	kind      int
	entity    *Entity // placeholder for create commands, target otherwise
	index     int
	component Component
	obsolete  bool
}

// entityState tracks per-entity commands so multiple commands touching the
// same component slot collapse into a single final-state change.
type entityState struct {
	components map[int]*command // latest write/remove command per component index
	destroy    *command         // pending destroy command, if any
	created    bool             // entity was created by this buffer
}

type commandBuffer struct {
	context  *entityBase
	commands []*command
	states   map[*Entity]*entityState
	flushing bool
	lastErr  error
}

// errFlushInProgress guards against re-entrant flushes, e.g. a group event
// callback triggered by Flush calling Flush again on the same buffer.
var errFlushInProgress = errors.New("command buffer: flush already in progress")

/////////////////////////////////////////////////////
// Constructor:
/////////////////////////////////////////////////////

func newCommandBuffer(context *entityBase) CommandBuffer {
	return &commandBuffer{
		context: context,
		states:  make(map[*Entity]*entityState),
	}
}

/////////////////////////////////////////////////////
// Public:
/////////////////////////////////////////////////////

func (b *commandBuffer) CreateEntity() *Entity {
	// A detached placeholder: it owns no live slot in the context, so groups
	// and collectors cannot see it until Flush creates the real entity.
	entity := createEntity(-1, &b.context.componentPool, b.context.componentTotal)
	entity.context = b.context
	entity.isAlive = false

	cmd := &command{kind: commandCreate, entity: entity}
	b.commands = append(b.commands, cmd)
	b.states[entity] = &entityState{
		components: make(map[int]*command),
		created:    true,
	}
	return entity
}

func (b *commandBuffer) DestroyEntity(e *Entity) error {
	if err := b.checkEntity(e); err != nil {
		return err
	}
	state := b.state(e)
	if state.destroy != nil {
		return ErrDuplicateDestroy
	}

	cmd := &command{kind: commandDestroy, entity: e}
	b.commands = append(b.commands, cmd)
	state.destroy = cmd

	// Every still-pending component change becomes meaningless: the entity
	// either disappears completely or, when it was created inside this same
	// buffer, is never created at all.
	for _, c := range state.components {
		c.obsolete = true
	}
	state.components = make(map[int]*command)
	return nil
}

func (b *commandBuffer) AddComponent(e *Entity, index int, component Component) error {
	return b.write(e, index, component)
}

func (b *commandBuffer) ReplaceComponent(e *Entity, index int, component Component) error {
	return b.write(e, index, component)
}

func (b *commandBuffer) RemoveComponent(e *Entity, index int) error {
	if err := b.checkEntity(e); err != nil {
		return err
	}
	if err := b.checkIndex(index); err != nil {
		return err
	}
	state := b.state(e)
	if state.destroy != nil {
		return ErrEntityPendingKill
	}
	if previous, ok := state.components[index]; ok && previous.kind == commandRemove {
		return ErrDuplicateRemove
	}

	cmd := &command{kind: commandRemove, entity: e, index: index}
	b.commands = append(b.commands, cmd)

	// A queued write is cancelled by the removal; an earlier removal is
	// superseded as well so only one remove command reaches Flush.
	if previous, ok := state.components[index]; ok {
		previous.obsolete = true
	}
	state.components[index] = cmd
	return nil
}

func (b *commandBuffer) Flush() error {
	if len(b.commands) == 0 {
		// Repeated flushes are defined as a successful no-op.
		b.Reset()
		return nil
	}
	if b.flushing {
		return FlushErrors{errFlushInProgress}
	}
	b.flushing = true
	defer func() {
		b.flushing = false
	}()

	var flushErrs FlushErrors

	// Placeholder -> real entity, resolved once per create command.
	created := make(map[*Entity]*Entity, len(b.states))

	for _, cmd := range b.commands {
		if cmd.obsolete {
			continue
		}

		target := cmd.entity
		if real, ok := created[target]; ok {
			target = real
		}

		switch cmd.kind {
		case commandCreate:
			real := b.context.CreateEntity()
			created[cmd.entity] = real

		case commandDestroy:
			if target == nil || target.context != b.context {
				flushErrs = append(flushErrs, ErrForeignEntity)
				continue
			}
			if !target.isAlive {
				// Already destroyed outside the buffer: nothing to do, but the
				// caller asked for an explicit outcome.
				flushErrs = append(flushErrs, ErrEntityDestroyed)
				continue
			}
			target.Destroy()

		case commandWrite:
			if target == nil || target.context != b.context {
				flushErrs = append(flushErrs, ErrForeignEntity)
				continue
			}
			if !target.isAlive {
				flushErrs = append(flushErrs, ErrEntityDestroyed)
				continue
			}
			if cmd.component == nil {
				flushErrs = append(flushErrs, ErrNilComponent)
				continue
			}
			// Replace semantics, identical to Entity.Replace / generated
			// ReplaceX: add on an empty slot, replace otherwise. The group and
			// collector events fired here are the only notifications, hence
			// intermediate buffered states never leak.
			target.Replace(cmd.index, cmd.component)

		case commandRemove:
			if target == nil || target.context != b.context {
				flushErrs = append(flushErrs, ErrForeignEntity)
				continue
			}
			if !target.isAlive {
				flushErrs = append(flushErrs, ErrEntityDestroyed)
				continue
			}
			if target.Get(cmd.index) == nil {
				// Removing an absent component is a no-op by final state.
				continue
			}
			target.Remove(cmd.index)
		}
	}

	b.Reset()

	if len(flushErrs) == 0 {
		return nil
	}
	return flushErrs
}

func (b *commandBuffer) Reset() {
	// Keep the backing arrays so large frames reuse their capacity.
	b.commands = b.commands[:0]
	b.states = make(map[*Entity]*entityState)
	b.lastErr = nil
}

func (b *commandBuffer) Len() int {
	return len(b.commands)
}

func (b *commandBuffer) Executer() {
	b.lastErr = b.Flush()
}

func (b *commandBuffer) LastError() error {
	return b.lastErr
}

/////////////////////////////////////////////////////
// Private:
/////////////////////////////////////////////////////

func (b *commandBuffer) write(e *Entity, index int, component Component) error {
	if err := b.checkEntity(e); err != nil {
		return err
	}
	if err := b.checkIndex(index); err != nil {
		return err
	}
	if component == nil {
		return ErrNilComponent
	}
	state := b.state(e)
	if state.destroy != nil {
		return ErrEntityPendingKill
	}

	cmd := &command{kind: commandWrite, entity: e, index: index, component: component}
	b.commands = append(b.commands, cmd)

	// Add and Replace converge on one last-write-wins slot, matching the
	// generated component API where only the final component value matters.
	if previous, ok := state.components[index]; ok {
		previous.obsolete = true
	}
	state.components[index] = cmd
	return nil
}

func (b *commandBuffer) checkEntity(e *Entity) error {
	if e == nil {
		return ErrNilEntity
	}
	if e.context != b.context {
		return ErrForeignEntity
	}
	if !e.isAlive {
		// Placeholders created through this buffer are not alive yet but are
		// valid command targets; everything else dead has been destroyed.
		if state, ok := b.states[e]; ok && state.created {
			return nil
		}
		return ErrEntityDestroyed
	}
	return nil
}

func (b *commandBuffer) checkIndex(index int) error {
	if index < 0 || index >= b.context.componentTotal {
		return ErrComponentIndex
	}
	return nil
}

func (b *commandBuffer) state(e *Entity) *entityState {
	state, ok := b.states[e]
	if !ok {
		state = &entityState{components: make(map[int]*command)}
		b.states[e] = state
	}
	return state
}

// FlushErrors is returned by Flush when one or more commands could not be
// applied. Commands are independent: a failed command never stops the
// remaining ones, and every failure is reported.
type FlushErrors []error

func (fe FlushErrors) Error() string {
	if len(fe) == 0 {
		return "command buffer: no errors"
	}
	msg := "command buffer: "
	for i, err := range fe {
		if i > 0 {
			msg += "; "
		}
		msg += err.Error()
	}
	return msg
}

// Has reports whether the given error is among the flush failures.
func (fe FlushErrors) Has(target error) bool {
	for _, err := range fe {
		if err == target {
			return true
		}
	}
	return false
}
