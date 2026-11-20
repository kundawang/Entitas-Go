# Entitas-Go

Entitas-GO is a fast Entity Component System Framework (ECS) Go 1.17 port of [Entitas v1.13.0 for C# and Unity.](https://github.com/sschmid/Entitas-CSharp)

# Code Generator

1) Install the library in your project

```
go get github.com/Falldot/Entitas-Go
```

2) Create context file, with components.
```
//Important! The file should contain this line
//go:generate go run github.com/Falldot/Entitas-Go
```

### Game.go
```golang
package main

//go:generate go run github.com/Falldot/Entitas-Go

type Position struct {
	X, Y float64
}

type Direction struct {
	X, Y float64
}

type Speed int

type Health int

type Sprite struct {
	tag  string
	W, H int
}
```

3) Use
```
go generate
```

4) A folder 'Entitas' will be created in your project, please do not edit it

5) Edit the context file, add methods and components, and then use again `go generate`

# Examples

## Entity
```go
// main.go file
package main

import (
	ecs "myProject/Entitas"
)

func main() {
	contexts := ecs.CreateContexts()
    game := contexts.Game()

    // System registration
    systems := ecs.CreateSystemPool()
    systems.Add(&Translate{})
    systems.Add(&ReactiveTranslate{})

    // Create entity
    player := game.CreateEntity()

    // Add component
	player.AddPosition(10, 30)
	player.AddDirection(0, 0)
	player.AddSpeed(5)

    // Remove component
    player.RemoveSpeed()

    // Replace component
    player.ReplacePosition(30, 10)

    // On or Off component
    player.OffDirection()
    player.OnDirection()

    // Destroy entity
    player.Destroy()

    // GameLoop
    systems.Init(contexts)
    for true {
        systems.Execute()
        systems.Clean()
    }
    systems.Exit(contexts)
}
```

## System
```go
type Translate struct {
    group ecs.Group
}

func (s *Translate) Initer(contexts ecs.Contexts) {
	game := contexts.Game()
    s.group = game.Group(ecs.NewMatcher().AllOf(ecs.Position))
}

func (s *Translate) Executer() {
	for _, e := range s.group.GetEntities() {
        pos := e.GetPosition()
		e.ReplacePosition(pos.X + 10, pos.X + 10)
	}
}
```

## Reactive system
```go
type ReactiveTranslate struct {
}

func (s *ReactiveTranslate) Trigger(contexts ecs.Contexts) ecs.Collector {
	game := contexts.Game()
	return game.Collector(ecs.NewMatcher().AllOf(ecs.Position)).OnUpdate().OnAdd()
}

func (s *ReactiveTranslate) Filter(entity *ecs.Entity) bool {
	return entity.Has(ecs.Position)
}

func (s *ReactiveTranslate) Executer(entities []*ecs.Entity) {
	for _, e := range entities {
		pos := e.GetPosition()
		e.ReplacePosition(pos.X+10, pos.X+10)
	}
}
```

## Command buffer (deferred entity changes)
Destroying enemies or spawning projectiles while iterating a group mutates the
very set being iterated, so members of the current frame can be skipped. A
`CommandBuffer` records changes and applies them all at once at the end of the
frame via `Flush()`. Until then every group and collector keeps its old state,
so iteration is always safe.

```go
type BattleSystem struct {
	enemies ecs.Group
	buffer  ecs.CommandBuffer
}

func (s *BattleSystem) Initer(contexts ecs.Contexts) {
	game := contexts.Game()
	s.enemies = game.Group(ecs.NewMatcher().AllOf(ecs.Health))
	s.buffer = game.CreateCommandBuffer()
}

func (s *BattleSystem) Executer() {
	for _, e := range s.enemies.GetEntities() {
		if e.GetHealth() <= 0 {
			// Deferred creation: the returned placeholder accepts component
			// commands; the real entity appears in the context at Flush time.
			shot := s.buffer.CreateEntity()
			s.buffer.AddComponent(shot, ecs.Sprite, &SpriteComponent{})
			s.buffer.DestroyEntity(e) // deferred destruction
		}
	}
	s.buffer.Flush() // all changes applied in enqueue order
}
```

Recording API:

| Command | Effect at `Flush()` |
| --- | --- |
| `CreateEntity()` | Returns a placeholder; the real entity is created in the context |
| `DestroyEntity(e)` | Destroys the entity and removes it from every group |
| `AddComponent(e, i, c)` | Adds `c`; same-slot writes coalesce to the last value |
| `ReplaceComponent(e, i, c)` | Adds when the slot is empty, replaces otherwise (`Entity.Replace` semantics) |
| `RemoveComponent(e, i)` | Removes the component; cancels a queued write for the same slot |
| `Reset()` | Discards all recorded commands without applying them |

Semantics and guarantees:

- Commands are applied in enqueue order. Multiple commands touching one entity
  and one component slot are coalesced, so after `Flush()` groups and
  collectors only observe the entity's final state (the generated `AddX` /
  `ReplaceX` / `RemoveX` APIs follow the same rules).
- Recording validates its arguments and returns a sentinel error for nil
  entities, cross-context references, already destroyed entities, out-of-range
  component indices, nil components, duplicate destroys/removes, and commands
  queued after a destroy.
- During `Flush()`, commands are independent: a command that can no longer be
  applied is collected into a `FlushErrors` result while the remaining commands
  still run. `Flush()` on an empty (or already flushed) buffer is a successful
  no-op; the buffer resets itself and can be reused.
- `CreateEntity`/`DestroyEntity` interleave safely in large batches — all
  bookkeeping is map/slice based, with no quadratic scans.
- A `CommandBuffer` also implements `Executer`, so it can be added to a system
  pool to flush automatically; inspect `LastError()` afterwards.

The MIT License (MIT)
=====================

Copyright © 2021 Vladislav Fedotov (Falldot)

Permission is hereby granted, free of charge, to any person
obtaining a copy of this software and associated documentation
files (the “Software”), to deal in the Software without
restriction, including without limitation the rights to use,
copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the
Software is furnished to do so, subject to the following
conditions:

The above copyright notice and this permission notice shall be
included in all copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED “AS IS”, WITHOUT WARRANTY OF ANY KIND,
EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES
OF MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE AND
NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR COPYRIGHT
HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY,
WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING
FROM, OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR
OTHER DEALINGS IN THE SOFTWARE.