// Package dynamodb is a thin, ergonomic wrapper around AWS
// DynamoDB. It builds on `providers/aws` for credential resolution
// and exposes typed CRUD + Query/Scan + transaction primitives.
//
// Two API levels:
//
//   - Low level: `Item map[string]types.AttributeValue` for
//     callers who need DDB-native semantics (numeric, set types,
//     etc.).
//   - High level: typed `Marshal[T]` / `Unmarshal[T]` helpers
//     using `attributevalue` so handlers can deal in plain
//     Go structs.
package dynamodb

import (
	"context"
	"errors"
	"fmt"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	mkawsutil "mkfst/providers/aws"
)

// ddbAPI is the narrow surface of the DynamoDB SDK we actually
// call. Defining it as an interface lets tests substitute a
// mockery-generated mock (see mock_ddb_api_test.go) instead of
// going to a real AWS endpoint. The real *dynamodb.Client
// satisfies it implicitly.
type ddbAPI interface {
	GetItem(ctx context.Context, in *dynamodb.GetItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	PutItem(ctx context.Context, in *dynamodb.PutItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	UpdateItem(ctx context.Context, in *dynamodb.UpdateItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
	DeleteItem(ctx context.Context, in *dynamodb.DeleteItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error)
	Query(ctx context.Context, in *dynamodb.QueryInput, opts ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error)
	Scan(ctx context.Context, in *dynamodb.ScanInput, opts ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error)
}

// Client wraps the AWS DynamoDB client.
type Client struct {
	cli ddbAPI
	cfg awssdk.Config
}

// Opts configures NewClient.
type Opts struct {
	// AWS reuses providers/aws.Opts for credential resolution.
	AWS mkawsutil.Opts
}

// New builds a Client. Resolves credentials via providers/aws.
func New(ctx context.Context, opts Opts) (*Client, error) {
	cfg, err := mkawsutil.Resolve(ctx, opts.AWS)
	if err != nil {
		return nil, err
	}
	return &Client{cli: dynamodb.NewFromConfig(cfg), cfg: cfg}, nil
}

// FromConfig wraps an existing AWS config (for sharing across
// providers in the same auth scope).
func FromConfig(cfg awssdk.Config) *Client {
	return &Client{cli: dynamodb.NewFromConfig(cfg), cfg: cfg}
}

// fromAPI is the test-only constructor: builds a Client around a
// mock ddbAPI.
func fromAPI(api ddbAPI) *Client {
	return &Client{cli: api}
}

// === low-level Item-shaped API ===

// Item is the native DDB representation: attribute name → typed
// AttributeValue.
type Item = map[string]types.AttributeValue

// Get fetches a single item by primary key.
//
// Returns (nil, nil) when no item matches the key (404 is not an
// error). Returns (nil, err) on any other failure.
func (c *Client) Get(ctx context.Context, table string, key Item) (Item, error) {
	if table == "" {
		return nil, errors.New("dynamodb.Get: table required")
	}
	if len(key) == 0 {
		return nil, errors.New("dynamodb.Get: key required")
	}
	res, err := c.cli.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: awssdk.String(table),
		Key:       key,
	})
	if err != nil {
		return nil, fmt.Errorf("dynamodb.Get: %w", err)
	}
	if len(res.Item) == 0 {
		return nil, nil
	}
	return res.Item, nil
}

// Put writes an item. Existing item under the same key is
// overwritten; use a ConditionExpression via SDK for "create only"
// semantics.
func (c *Client) Put(ctx context.Context, table string, item Item) error {
	if table == "" {
		return errors.New("dynamodb.Put: table required")
	}
	if len(item) == 0 {
		return errors.New("dynamodb.Put: item required")
	}
	_, err := c.cli.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: awssdk.String(table),
		Item:      item,
	})
	if err != nil {
		return fmt.Errorf("dynamodb.Put: %w", err)
	}
	return nil
}

// UpdateOpts configures Update.
type UpdateOpts struct {
	// Updates is a map of attribute name → new value. Each entry
	// is applied via SET in the UpdateExpression.
	Updates Item
	// Removes lists attribute names that should be REMOVE'd.
	Removes []string
	// ConditionExpression, when non-empty, gates the update.
	ConditionExpression string
	// ExpressionAttributeNames / Values map placeholders used in
	// the condition.
	ExpressionAttributeNames  map[string]string
	ExpressionAttributeValues Item
}

// Update applies a partial update to the item identified by key.
func (c *Client) Update(ctx context.Context, table string, key Item, opts UpdateOpts) error {
	if table == "" {
		return errors.New("dynamodb.Update: table required")
	}
	if len(key) == 0 {
		return errors.New("dynamodb.Update: key required")
	}
	if len(opts.Updates) == 0 && len(opts.Removes) == 0 {
		return errors.New("dynamodb.Update: nothing to update or remove")
	}
	updateExpr, exprNames, exprVals := buildUpdateExpression(opts.Updates, opts.Removes)
	for k, v := range opts.ExpressionAttributeNames {
		exprNames[k] = v
	}
	for k, v := range opts.ExpressionAttributeValues {
		exprVals[k] = v
	}
	in := &dynamodb.UpdateItemInput{
		TableName:                 awssdk.String(table),
		Key:                       key,
		UpdateExpression:          awssdk.String(updateExpr),
		ExpressionAttributeNames:  exprNames,
		ExpressionAttributeValues: exprVals,
	}
	if opts.ConditionExpression != "" {
		in.ConditionExpression = awssdk.String(opts.ConditionExpression)
	}
	if _, err := c.cli.UpdateItem(ctx, in); err != nil {
		return fmt.Errorf("dynamodb.Update: %w", err)
	}
	return nil
}

// Delete removes an item by primary key. Idempotent: deleting a
// non-existent item is not an error.
func (c *Client) Delete(ctx context.Context, table string, key Item) error {
	if table == "" {
		return errors.New("dynamodb.Delete: table required")
	}
	if len(key) == 0 {
		return errors.New("dynamodb.Delete: key required")
	}
	_, err := c.cli.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: awssdk.String(table),
		Key:       key,
	})
	if err != nil {
		return fmt.Errorf("dynamodb.Delete: %w", err)
	}
	return nil
}

// QueryOpts is a thin alias for the SDK input minus the table name.
// We expose it directly so callers can use the full DDB query
// vocabulary without us re-defining every field.
type QueryOpts = dynamodb.QueryInput

// Query runs a DDB Query. The caller fills in
// KeyConditionExpression, ExpressionAttributeNames/Values, IndexName,
// etc.; we set TableName from the first arg.
func (c *Client) Query(ctx context.Context, table string, opts QueryOpts) ([]Item, error) {
	if table == "" {
		return nil, errors.New("dynamodb.Query: table required")
	}
	in := opts
	in.TableName = awssdk.String(table)
	out, err := c.cli.Query(ctx, &in)
	if err != nil {
		return nil, fmt.Errorf("dynamodb.Query: %w", err)
	}
	return out.Items, nil
}

// ScanOpts mirrors QueryOpts.
type ScanOpts = dynamodb.ScanInput

// Scan reads every matching item. Use sparingly — full-table
// scans are expensive.
func (c *Client) Scan(ctx context.Context, table string, opts ScanOpts) ([]Item, error) {
	if table == "" {
		return nil, errors.New("dynamodb.Scan: table required")
	}
	in := opts
	in.TableName = awssdk.String(table)
	out, err := c.cli.Scan(ctx, &in)
	if err != nil {
		return nil, fmt.Errorf("dynamodb.Scan: %w", err)
	}
	return out.Items, nil
}

// === typed (struct-based) helpers ===

// MarshalItem converts a Go struct to a DDB Item. Field tags
// follow the standard `dynamodbav:"..."` convention from
// aws-sdk-go-v2/feature/dynamodb/attributevalue.
func MarshalItem(in any) (Item, error) {
	return attributevalue.MarshalMap(in)
}

// UnmarshalItem converts a DDB Item back to a Go struct.
func UnmarshalItem(item Item, out any) error {
	return attributevalue.UnmarshalMap(item, out)
}

// UnmarshalItems is the slice form. items[i] → out[i].
func UnmarshalItems(items []Item, out any) error {
	return attributevalue.UnmarshalListOfMaps(items, out)
}

// PutTyped is a typed convenience: marshal + Put.
func (c *Client) PutTyped(ctx context.Context, table string, in any) error {
	item, err := MarshalItem(in)
	if err != nil {
		return fmt.Errorf("dynamodb.PutTyped: marshal: %w", err)
	}
	return c.Put(ctx, table, item)
}

// GetTyped is a typed convenience: Get + unmarshal. Returns
// false on miss.
func (c *Client) GetTyped(ctx context.Context, table string, key Item, out any) (bool, error) {
	item, err := c.Get(ctx, table, key)
	if err != nil {
		return false, err
	}
	if item == nil {
		return false, nil
	}
	if err := UnmarshalItem(item, out); err != nil {
		return false, fmt.Errorf("dynamodb.GetTyped: unmarshal: %w", err)
	}
	return true, nil
}

// === update-expression builder ===
//
// Constructs the UpdateExpression / ExpressionAttributeNames /
// ExpressionAttributeValues triple from `updates` (SET) and
// `removes` (REMOVE). Names are placeholdered as #n0, #n1, ...;
// values as :v0, :v1, ... — keeps reserved words and special
// characters out of harm's way.

func buildUpdateExpression(updates Item, removes []string) (expr string, names map[string]string, vals Item) {
	names = map[string]string{}
	vals = Item{}
	var setParts, removeParts []string
	i := 0
	for k, v := range updates {
		nameKey := fmt.Sprintf("#n%d", i)
		valKey := fmt.Sprintf(":v%d", i)
		names[nameKey] = k
		vals[valKey] = v
		setParts = append(setParts, nameKey+" = "+valKey)
		i++
	}
	for _, r := range removes {
		nameKey := fmt.Sprintf("#n%d", i)
		names[nameKey] = r
		removeParts = append(removeParts, nameKey)
		i++
	}
	if len(setParts) > 0 {
		expr = "SET " + joinComma(setParts)
	}
	if len(removeParts) > 0 {
		if expr != "" {
			expr += " "
		}
		expr += "REMOVE " + joinComma(removeParts)
	}
	return expr, names, vals
}

func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}
