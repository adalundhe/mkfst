package tonic

import "reflect"

// Container is the dependency-injection registry used by Handler to resolve
// handler arguments. Deps are looked up by exact type match. Register a value
// once at startup; the same value is injected into every handler that asks
// for that type.
//
// Typed nils are allowed (e.g. (*sql.DB)(nil)) — a handler signature can list
// the type without forcing the dep to actually exist. The handler must not
// dereference what it didn't request to be live.
type Container struct {
	deps map[reflect.Type]reflect.Value
}

// NewContainer returns a Container pre-populated with the given deps.
func NewContainer(deps ...interface{}) *Container {
	c := &Container{deps: make(map[reflect.Type]reflect.Value)}
	c.Register(deps...)
	return c
}

// Register stores each dep under its concrete type. Untyped nil is skipped.
// A later Register for the same type overwrites the previous value.
func (c *Container) Register(deps ...interface{}) {
	for _, d := range deps {
		v := reflect.ValueOf(d)
		if !v.IsValid() {
			continue
		}
		c.deps[reflect.TypeOf(d)] = v
	}
}

// RegisterAs stores dep under the supplied interface or alias type. Useful for
// registering a concrete *Impl as InterfaceX so handlers can ask for
// InterfaceX directly.
func (c *Container) RegisterAs(t reflect.Type, dep interface{}) {
	v := reflect.ValueOf(dep)
	if !v.IsValid() {
		return
	}
	c.deps[t] = v
}

// Lookup returns the registered value for t and whether it was found.
func (c *Container) Lookup(t reflect.Type) (reflect.Value, bool) {
	if c == nil {
		return reflect.Value{}, false
	}
	v, ok := c.deps[t]
	return v, ok
}

// Has reports whether t is registered.
func (c *Container) Has(t reflect.Type) bool {
	_, ok := c.Lookup(t)
	return ok
}
