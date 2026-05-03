package tonic

import (
	"fmt"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	validator "github.com/go-playground/validator/v10"
	"github.com/google/uuid"
)

var (
	validatorObj  *validator.Validate
	validatorOnce sync.Once
)

// callPlan caches the per-handler reflection work done at registration time:
// which container values to inject as positional args, and the optional input
// struct type to bind from the request.
type callPlan struct {
	deps      []reflect.Value
	inputType reflect.Type
}

// Handler returns a Gin HandlerFunc wrapping h.
//
// The handler signature must be:
//
//	func(*gin.Context, ...deps, [*InputStruct]) ([Output], error)
//
// where each dep type must be registered in container. The optional last arg
// must be a pointer to a struct and is bound from the request (body / query /
// path / header) before the call.
//
// Handler panics if the signature can't be reconciled with the container.
func Handler(h interface{}, container *Container, status int, options ...func(*Route)) gin.HandlerFunc {
	if container == nil {
		container = NewContainer()
	}

	hv := reflect.ValueOf(h)
	if hv.Kind() != reflect.Func {
		panic(fmt.Sprintf("handler parameters must be a function, got %T", h))
	}
	ht := hv.Type()
	fname := fmt.Sprintf("%s_%s", runtime.FuncForPC(hv.Pointer()).Name(), uuid.Must(uuid.NewRandom()).String())

	plan := buildCallPlan(ht, container, fname)
	out := output(ht, fname)

	// Wrap Gin handler.
	f := func(c *gin.Context, ct *Container) {
		_, ok := c.Get(tonicWantRouteInfos)
		if ok {
			r := &Route{}
			r.defaultStatusCode = status
			r.handler = hv
			r.handlerType = ht
			r.inputType = plan.inputType
			r.outputType = out
			for _, opt := range options {
				opt(r)
			}
			c.Set(tonicRoutesInfos, r)
			c.Abort()
			return
		}

		args := make([]reflect.Value, 0, 1+len(plan.deps)+1)
		args = append(args, reflect.ValueOf(c))
		args = append(args, plan.deps...)

		if plan.inputType != nil {
			input := reflect.New(plan.inputType)
			if err := bindHook(c, ct, input.Interface()); err != nil {
				handleError(c, BindError{message: err.Error(), typ: plan.inputType})
				return
			}
			if err := bind(c, input, QueryTag, extractQuery); err != nil {
				handleError(c, err)
				return
			}
			if err := bind(c, input, PathTag, extractPath); err != nil {
				handleError(c, err)
				return
			}
			if err := bind(c, input, HeaderTag, extractHeader); err != nil {
				handleError(c, err)
				return
			}
			initValidator()
			args = append(args, input)
			if err := validatorObj.Struct(input.Interface()); err != nil {
				handleError(c, BindError{message: err.Error(), validationErr: err})
				return
			}
		}

		var err, val interface{}
		ret := hv.Call(args)
		if out != nil {
			val = ret[0].Interface()
			err = ret[1].Interface()
		} else {
			err = ret[0].Interface()
		}
		if err != nil {
			handleError(c, err.(error))
			return
		}
		renderHook(c, status, val)
	}

	route := &Route{
		defaultStatusCode: status,
		handler:           hv,
		handlerType:       ht,
		inputType:         plan.inputType,
		outputType:        out,
	}
	for _, opt := range options {
		opt(route)
	}
	routesMu.Lock()
	routes[fname] = route
	routesMu.Unlock()

	ret := func(c *gin.Context) { execHook(c, container, f, fname) }

	funcsMu.Lock()
	defer funcsMu.Unlock()
	funcs[runtime.FuncForPC(reflect.ValueOf(ret).Pointer()).Name()] = struct{}{}

	return ret
}

// RegisterValidation registers a custom validation on the validator.Validate instance of the package
// NOTE: calling this function may instantiate the validator itself.
// NOTE: this function is not thread safe, since the validator validation registration isn't
func RegisterValidation(tagName string, validationFunc validator.Func) error {
	initValidator()
	return validatorObj.RegisterValidation(tagName, validationFunc)
}

// RegisterTagNameFunc registers a function to get alternate names for StructFields.
//
// eg. to use the names which have been specified for JSON representations of structs, rather than normal Go field names:
//
//	tonic.RegisterTagNameFunc(func(fld reflect.StructField) string {
//	    name := strings.SplitN(fld.Tag.Get("json"), ",", 2)[0]
//	    if name == "-" {
//	        return ""
//	    }
//	    return name
//	})
func RegisterTagNameFunc(registerTagFunc validator.TagNameFunc) {
	initValidator()
	validatorObj.RegisterTagNameFunc(registerTagFunc)
}

func initValidator() {
	validatorOnce.Do(func() {
		validatorObj = validator.New()
		validatorObj.SetTagName(ValidationTag)
	})
}

// bind binds the fields the fields of the input object in with
// the values of the parameters extracted from the Gin context.
// It reads tag to know what to extract using the extractor func.
func bind(c *gin.Context, v reflect.Value, tag string, extract extractor) error {
	t := v.Type()

	if t.Kind() == reflect.Ptr {
		t = t.Elem()
		v = v.Elem()
	}
	for i := 0; i < t.NumField(); i++ {
		ft := t.Field(i)
		field := v.Field(i)

		// Handle embedded fields with a recursive call.
		// If the field is a pointer, but is nil, we
		// create a new value of the same type, or we
		// take the existing memory address.
		if ft.Anonymous {
			if field.Kind() == reflect.Ptr {
				if field.IsNil() {
					field.Set(reflect.New(field.Type().Elem()))
				}
			} else {
				if field.CanAddr() {
					field = field.Addr()
				}
			}
			err := bind(c, field, tag, extract)
			if err != nil {
				return err
			}
			continue
		}
		tagValue := ft.Tag.Get(tag)
		if tagValue == "" {
			continue
		}
		// Set-up context for extractors.
		// Query.
		c.Set(ExplodeTag, true) // default
		if explodeVal, ok := ft.Tag.Lookup(ExplodeTag); ok {
			if explode, err := strconv.ParseBool(explodeVal); err == nil && !explode {
				c.Set(ExplodeTag, false)
			}
		}
		_, fieldValues, err := extract(c, tagValue)
		if err != nil {
			return BindError{field: ft.Name, typ: t, message: err.Error()}
		}
		// Extract default value and use it in place
		// if no values were returned.
		def, ok := ft.Tag.Lookup(DefaultTag)
		if ok && len(fieldValues) == 0 {
			if c.GetBool(ExplodeTag) {
				fieldValues = append(fieldValues, strings.Split(def, ",")...)
			} else {
				fieldValues = append(fieldValues, def)
			}
		}
		if len(fieldValues) == 0 {
			continue
		}
		// If the field is a nil pointer to a concrete type,
		// create a new addressable value for this type.
		if field.Kind() == reflect.Ptr && field.IsNil() {
			f := reflect.New(field.Type().Elem())
			field.Set(f)
		}
		// Dereference pointer.
		if field.Kind() == reflect.Ptr {
			field = field.Elem()
		}
		kind := field.Kind()

		// Multiple values can only be filled to types
		// Slice and Array.
		if len(fieldValues) > 1 && (kind != reflect.Slice && kind != reflect.Array) {
			return BindError{field: ft.Name, typ: t, message: "multiple values not supported"}
		}
		// Ensure that the number of values to fill does
		// not exceed the length of a field of type Array.
		if kind == reflect.Array {
			if field.Len() != len(fieldValues) {
				return BindError{field: ft.Name, typ: t, message: fmt.Sprintf(
					"parameter expect %d values, got %d", field.Len(), len(fieldValues)),
				}
			}
		}
		if kind == reflect.Slice || kind == reflect.Array {
			// Create a new slice with an adequate
			// length to set all the values.
			if kind == reflect.Slice {
				field.Set(reflect.MakeSlice(field.Type(), 0, len(fieldValues)))
			}
			for i, val := range fieldValues {
				v := reflect.New(field.Type().Elem()).Elem()
				err = bindStringValue(val, v)
				if err != nil {
					return BindError{field: ft.Name, typ: t, message: err.Error()}
				}
				if kind == reflect.Slice {
					field.Set(reflect.Append(field, v))
				}
				if kind == reflect.Array {
					field.Index(i).Set(v)
				}
			}
			continue
		}
		// Handle enum values.
		enum := ft.Tag.Get(EnumTag)
		if enum != "" {
			enumValues := strings.Split(strings.TrimSpace(enum), ",")
			if len(enumValues) != 0 {
				if !contains(enumValues, fieldValues[0]) {
					return BindError{field: ft.Name, typ: t, message: fmt.Sprintf(
						"parameter has not an acceptable value, %s=%v", EnumTag, enumValues),
					}
				}
			}
		}
		// Fill string value into input field.
		err = bindStringValue(fieldValues[0], field)
		if err != nil {
			return BindError{field: ft.Name, typ: t, message: err.Error()}
		}
	}
	return nil
}

// buildCallPlan validates the handler signature against container and returns
// the per-call plan: pre-resolved deps in arg order, plus the optional input
// type. Panics on any incompatibility.
//
// Rules:
//   - arg 0 must be *gin.Context
//   - args 1..N: each looked up in container by exact type. The first arg that
//     isn't in the container must be the last arg AND a pointer to a struct,
//     in which case it becomes the bound input.
func buildCallPlan(ht reflect.Type, container *Container, name string) callPlan {
	n := ht.NumIn()
	if n < 1 {
		panic(fmt.Sprintf("handler %s must take at least *gin.Context", name))
	}
	if !ht.In(0).ConvertibleTo(reflect.TypeOf(&gin.Context{})) {
		panic(fmt.Sprintf(
			"invalid first parameter for handler %s, expected *gin.Context, got %v",
			name, ht.In(0),
		))
	}

	plan := callPlan{}
	for i := 1; i < n; i++ {
		argType := ht.In(i)
		if v, ok := container.Lookup(argType); ok {
			plan.deps = append(plan.deps, v)
			continue
		}
		// Not registered — only allowed as the final arg, and only if it
		// looks like an input struct.
		if i != n-1 {
			panic(fmt.Sprintf(
				"handler %s arg %d (%v): type not registered in container and not the final arg; register it via Container.Register or move input to the last position",
				name, i, argType,
			))
		}
		if argType.Kind() != reflect.Ptr || argType.Elem().Kind() != reflect.Struct {
			panic(fmt.Sprintf(
				"handler %s last arg must be a registered dep or a pointer to an input struct, got %v",
				name, argType,
			))
		}
		plan.inputType = argType.Elem()
	}
	return plan
}

// output checks the output parameters of a tonic handler
// and return the type of the return type, if any.
func output(ht reflect.Type, name string) reflect.Type {
	n := ht.NumOut()

	if n < 1 || n > 2 {
		panic(fmt.Sprintf(
			"incorrect number of output parameters for handler %s, expected 1 or 2, got %d",
			name, n,
		))
	}
	// Check the type of the error parameter, which
	// should always come last.
	if !ht.Out(n - 1).Implements(reflect.TypeOf((*error)(nil)).Elem()) {
		panic(fmt.Sprintf(
			"unsupported type for handler %s output parameter: expected error interface, got %v",
			name, ht.Out(n-1),
		))
	}
	if n == 2 {
		t := ht.Out(0)
		if t.Kind() == reflect.Ptr {
			t = t.Elem()
		}
		return t
	}
	return nil
}

// handleError handles any error raised during the execution
// of the wrapping gin-handler.
func handleError(c *gin.Context, err error) {
	if len(c.Errors) == 0 {
		c.Error(err)
	}
	code, resp := errorHook(c, err)
	renderHook(c, code, resp)
}

// contains returns whether in contain s.
func contains(in []string, s string) bool {
	for _, v := range in {
		if v == s {
			return true
		}
	}
	return false
}
