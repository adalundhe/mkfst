# Tonic Hooks

Tonic exposes four package-level hooks that control how requests are bound,
validated, rendered and how errors are mapped to HTTP responses. They are
process-wide singletons — set them once at startup, before you register any
routes.

| Hook         | Type                                                   | Default behaviour                          |
| ------------ | ------------------------------------------------------ | ------------------------------------------ |
| `BindHook`   | `func(*gin.Context, *sql.DB, interface{}) error`       | JSON / YAML body binding, 256 KiB max body |
| `RenderHook` | `func(*gin.Context, int, interface{})`                  | JSON response, indented in debug mode      |
| `ErrorHook`  | `func(*gin.Context, error) (int, interface{})`         | `400 {"error": "<msg>"}`                   |
| `ExecHook`   | `func(*gin.Context, *sql.DB, MkfstHandler, string)`   | Calls the wrapping handler                 |

Call the corresponding setter once before `service.Run`:

```go
import "mkfst/tonic"

tonic.SetBindHook(myBinder)
tonic.SetRenderHook(myRenderer, "application/json")
tonic.SetErrorHook(myErrors)
tonic.SetExecHook(myExec)
```

---

## `BindHook`

The bind hook reads the body. The default delegates to Gin's JSON binder
(or to a custom YAML binder when `Content-Type` is one of the YAML media
types). If you need a stricter or more lenient body limit, replace it:

```go
tonic.SetBindHook(tonic.DefaultBindingHookMaxBodyBytes(2 << 20)) // 2 MiB
```

A custom binder that decodes msgpack or skips body validation entirely
takes the same shape:

```go
tonic.SetBindHook(func(c *gin.Context, db *sql.DB, in interface{}) error {
    if c.Request.ContentLength == 0 || c.Request.Method == http.MethodGet {
        return nil
    }
    return msgpack.NewDecoder(c.Request.Body).Decode(in)
})
```

The hook only handles the body. Query, path and header binding always go
through the reflection extractors and run *after* the bind hook.

---

## `RenderHook`

The render hook writes the response. The default renders JSON
(`c.JSON(...)`), pretty-printing in debug mode. To switch the entire
service to YAML or MessagePack:

```go
tonic.SetRenderHook(func(c *gin.Context, status int, payload interface{}) {
    if c.Writer.Written() {
        status = c.Writer.Status() // honour any earlier write
    }
    if payload == nil {
        c.String(status, "")
        return
    }
    c.YAML(status, payload)
}, "application/yaml") // second arg becomes the media type in the OpenAPI spec
```

`fizz` reads the configured media type and emits it as `responses[*].content`
in the spec.

---

## `ErrorHook`

This is the hook you will customise most often. The default returns
`400 {"error": "<msg>"}` for *any* error, which is rarely what you want.

A reasonable production hook:

```go
type httpErr struct { code int; msg string }
func (e httpErr) Error() string { return e.msg }

func NotFound(msg string) error    { return httpErr{404, msg} }
func Forbidden(msg string) error   { return httpErr{403, msg} }
func Conflict(msg string) error    { return httpErr{409, msg} }
func ServerErr(err error) error    { return httpErr{500, err.Error()} }

tonic.SetErrorHook(func(c *gin.Context, err error) (int, interface{}) {
    var he httpErr
    if errors.As(err, &he) {
        return he.code, gin.H{"error": he.msg}
    }
    var be tonic.BindError
    if errors.As(err, &be) {
        // Validation failed — surface field-level errors.
        return 422, gin.H{
            "error":  be.Error(),
            "fields": be.ValidationErrors(),
        }
    }
    return 500, gin.H{"error": err.Error()}
})
```

`tonic.BindError` is what tonic returns when binding or validation fails;
its `ValidationErrors()` method gives you the structured
`validator.ValidationErrors` slice so you can render per-field problems.

---

## `ExecHook`

This is the lowest-level hook. The default just calls the wrapping
handler. Override it to add a panic recovery wrapper, request-level
metrics, request logging, or to inject deadlines:

```go
tonic.SetExecHook(func(c *gin.Context, db *sql.DB, h tonic.MkfstHandler, fname string) {
    start := time.Now()
    defer func() {
        if r := recover(); r != nil {
            log.Printf("panic in %s: %v", fname, r)
            c.AbortWithStatus(http.StatusInternalServerError)
        }
        log.Printf("%s %s -> %d (%s) handler=%s",
            c.Request.Method, c.Request.URL.Path,
            c.Writer.Status(), time.Since(start), fname)
    }()
    h(c, db)
})
```

`fname` is the function name of the user handler (with a UUID suffix to
keep duplicates unique). It is what shows up as the OpenAPI operation ID
unless you override it with `fizz.ID(...)`.

---

## Custom validation rules

The validator instance is also a singleton. Register custom rules and
custom field-name resolvers before `service.Run`:

```go
import (
    "reflect"
    "strings"

    "mkfst/tonic"
    validator "github.com/go-playground/validator/v10"
)

// Use json tag names in error messages.
tonic.RegisterTagNameFunc(func(fld reflect.StructField) string {
    name := strings.SplitN(fld.Tag.Get("json"), ",", 2)[0]
    if name == "-" {
        return ""
    }
    return name
})

// Custom rule.
tonic.RegisterValidation("not_admin", func(fl validator.FieldLevel) bool {
    return fl.Field().String() != "admin"
})
```

Then use the rule in a struct tag:

```go
type CreateUserInput struct {
    Name string `json:"name" validate:"required,not_admin"`
}
```
