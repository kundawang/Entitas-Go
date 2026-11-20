// Copyright (c) 2021 Vladislav Fedotov (Falldot)
// License: MIT License
// MIT License web page: https://opensource.org/licenses/MIT
package ecs

type Entity struct {
	id             int
	componentTotal int

	components []Component
	componentState
	componentPool *[]componentPool

	// context owns the entity and is used by CommandBuffer to reject
	// cross-context references. isAlive is false for destroyed entities
	// and for entities deferred by a CommandBuffer until Flush creates them.
	context *entityBase
	isAlive bool

	onComponentAdd     onComponent
	onComponentRemove  onComponent
	onComponentReplace onComponent
	onComponentOff     onComponent

	onEntityDestroy  onEntity
	onEntityReleased onEntity
}

////////////////////////////////////////////////////////
// Constructor
////////////////////////////////////////////////////////

func createEntity(id int, componentPool *[]componentPool, componentTotal int) *Entity {
	return &Entity{
		id:             id,
		componentPool:  componentPool,
		components:     make([]Component, componentTotal),
		componentTotal: componentTotal,
		componentState: make(componentState, componentTotal),
	}
}

////////////////////////////////////////////////////////
// Public
////////////////////////////////////////////////////////

func (e *Entity) ID() int {
	return e.id
}

// IsAlive reports whether the entity is currently usable. It is false after
// Destroy() and for entities deferred by a CommandBuffer before Flush().
func (e *Entity) IsAlive() bool {
	return e.isAlive
}

func (e *Entity) Destroy() {
	e.isAlive = false
	e.componentState = make(componentState, e.componentTotal)
	e.onComponentOff = e.onComponentOff[:1]
	e.onComponentAdd = e.onComponentAdd[:1]
	e.onComponentReplace = e.onComponentReplace[:1]
	e.onComponentRemove = e.onComponentRemove[:2]
	e.onEntityDestroy.Execute(e)
}

func (e *Entity) OnComponentAdd(action func(*Entity, int, Component)) {
	e.onComponentAdd.On(action)
}

func (e *Entity) OnComponentRemoved(action func(*Entity, int, Component)) {
	e.onComponentRemove.On(action)
}

func (e *Entity) OnComponentReplaced(action func(*Entity, int, Component)) {
	e.onComponentReplace.On(action)
}

func (e *Entity) OnComponentOff(action func(*Entity, int, Component)) {
	e.onComponentOff.On(action)
}

func (e *Entity) OnEntityDestroy(action func(*Entity)) {
	e.onEntityDestroy.On(action)
}

func (e *Entity) OnEntityReleased(action func(*Entity)) {
	e.onEntityReleased.On(action)
}

func (e *Entity) Has(tp ...int) bool {
	for _, v := range tp {
		if e.componentState.onOff(v, e.componentTotal) {
			return false
		}
	}
	return true
}

func (e *Entity) Any(tp ...int) bool {
	for _, v := range tp {
		if !e.componentState.onOff(v, e.componentTotal) {
			return true
		}
	}
	return false
}

func (e *Entity) On(tp int) {
	e.componentState.On(tp, e.componentTotal)
}

func (e *Entity) Off(tp int) {
	e.componentState.Off(tp, e.componentTotal)
}

func (e *Entity) Remove(tp int) {
	e.onComponentRemove.Execute(e, tp, e.components[tp])
}

func (e *Entity) Get(tp int) Component {
	return e.components[tp]
}

func (e *Entity) Add(tp int, component Component) {
	e.components[tp] = component
	e.onComponentAdd.Execute(e, tp, component)
}

func (e *Entity) Replace(tp int, component Component) {
	if e.components[tp] == nil {
		e.Add(tp, component)
	} else {
		e.components[tp] = component
		e.onComponentReplace.Execute(e, tp, component)
	}
}

func (e *Entity) Create(tp int) (Component, bool) {
	return (*e.componentPool)[tp].Pop()
}
