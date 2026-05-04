package dynamodb

import (
	"context"
	"errors"
	"strings"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/stretchr/testify/mock"
)

// === Mock-backed CRUD tests ===
//
// Each test wires a mockddbAPI behind the Client and asserts both
// the SDK input we send AND the result we surface from the SDK
// output. Together with the pure-Go expression-builder tests
// below, this gives us coverage of the full request/response
// path without ever hitting AWS.

func sKey(v string) types.AttributeValue { return &types.AttributeValueMemberS{Value: v} }

func TestClient_Get_Hit(t *testing.T) {
	api := newMockddbAPI(t)
	c := fromAPI(api)

	api.EXPECT().GetItem(mock.Anything, mock.MatchedBy(func(in *dynamodb.GetItemInput) bool {
		return awssdk.ToString(in.TableName) == "Users" &&
			in.Key["id"].(*types.AttributeValueMemberS).Value == "u-1"
	})).Return(&dynamodb.GetItemOutput{
		Item: Item{
			"id":   sKey("u-1"),
			"name": sKey("alice"),
		},
	}, nil).Once()

	item, err := c.Get(context.Background(), "Users", Item{"id": sKey("u-1")})
	if err != nil {
		t.Fatal(err)
	}
	if item == nil || item["name"].(*types.AttributeValueMemberS).Value != "alice" {
		t.Fatalf("got %v, want alice", item)
	}
}

func TestClient_Get_Miss(t *testing.T) {
	api := newMockddbAPI(t)
	c := fromAPI(api)

	api.EXPECT().GetItem(mock.Anything, mock.Anything).
		Return(&dynamodb.GetItemOutput{ /* empty item map */ }, nil).Once()

	item, err := c.Get(context.Background(), "Users", Item{"id": sKey("missing")})
	if err != nil {
		t.Fatal(err)
	}
	if item != nil {
		t.Fatalf("expected nil item on miss, got %v", item)
	}
}

func TestClient_Get_PropagatesSDKError(t *testing.T) {
	api := newMockddbAPI(t)
	c := fromAPI(api)
	api.EXPECT().GetItem(mock.Anything, mock.Anything).
		Return(nil, errors.New("throttled")).Once()

	_, err := c.Get(context.Background(), "Users", Item{"id": sKey("u-1")})
	if err == nil || !strings.Contains(err.Error(), "throttled") {
		t.Fatalf("expected throttled error, got %v", err)
	}
}

func TestClient_Get_RejectsEmptyTable(t *testing.T) {
	c := fromAPI(newMockddbAPI(t))
	_, err := c.Get(context.Background(), "", Item{"id": sKey("u-1")})
	if err == nil || !strings.Contains(err.Error(), "table required") {
		t.Fatalf("expected table-required, got %v", err)
	}
}

func TestClient_Get_RejectsEmptyKey(t *testing.T) {
	c := fromAPI(newMockddbAPI(t))
	_, err := c.Get(context.Background(), "Users", Item{})
	if err == nil || !strings.Contains(err.Error(), "key required") {
		t.Fatalf("expected key-required, got %v", err)
	}
}

func TestClient_Put_BuildsCorrectInput(t *testing.T) {
	api := newMockddbAPI(t)
	c := fromAPI(api)

	api.EXPECT().PutItem(mock.Anything, mock.MatchedBy(func(in *dynamodb.PutItemInput) bool {
		return awssdk.ToString(in.TableName) == "Orders" &&
			in.Item["id"].(*types.AttributeValueMemberS).Value == "o-1"
	})).Return(&dynamodb.PutItemOutput{}, nil).Once()

	if err := c.Put(context.Background(), "Orders", Item{
		"id":  sKey("o-1"),
		"sku": sKey("WIDGET"),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestClient_Update_ConstructsExpression(t *testing.T) {
	api := newMockddbAPI(t)
	c := fromAPI(api)

	var captured *dynamodb.UpdateItemInput
	api.EXPECT().UpdateItem(mock.Anything, mock.Anything).
		Run(func(ctx context.Context, in *dynamodb.UpdateItemInput, opts ...func(*dynamodb.Options)) {
			captured = in
		}).
		Return(&dynamodb.UpdateItemOutput{}, nil).Once()

	err := c.Update(context.Background(), "Users",
		Item{"id": sKey("u-1")},
		UpdateOpts{
			Updates: Item{"name": sKey("alice"), "age": &types.AttributeValueMemberN{Value: "30"}},
			Removes: []string{"obsolete"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if captured == nil {
		t.Fatal("UpdateItem not called")
	}
	expr := awssdk.ToString(captured.UpdateExpression)
	if !strings.Contains(expr, "SET ") || !strings.Contains(expr, "REMOVE ") {
		t.Fatalf("expression missing clauses: %q", expr)
	}
	// Two SET attrs + one REMOVE attr → 3 placeholder names + 2 placeholder values.
	if len(captured.ExpressionAttributeNames) != 3 {
		t.Fatalf("expected 3 attribute name placeholders, got %d (%v)",
			len(captured.ExpressionAttributeNames), captured.ExpressionAttributeNames)
	}
	if len(captured.ExpressionAttributeValues) != 2 {
		t.Fatalf("expected 2 value placeholders, got %d", len(captured.ExpressionAttributeValues))
	}
}

func TestClient_Update_HonorsConditionExpression(t *testing.T) {
	api := newMockddbAPI(t)
	c := fromAPI(api)

	var captured *dynamodb.UpdateItemInput
	api.EXPECT().UpdateItem(mock.Anything, mock.Anything).
		Run(func(ctx context.Context, in *dynamodb.UpdateItemInput, opts ...func(*dynamodb.Options)) {
			captured = in
		}).
		Return(&dynamodb.UpdateItemOutput{}, nil).Once()

	err := c.Update(context.Background(), "Users",
		Item{"id": sKey("u-1")},
		UpdateOpts{
			Updates:             Item{"name": sKey("bob")},
			ConditionExpression: "attribute_exists(id)",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if awssdk.ToString(captured.ConditionExpression) != "attribute_exists(id)" {
		t.Fatalf("condition not propagated: %v", captured.ConditionExpression)
	}
}

func TestClient_Update_RejectsEmptyOpts(t *testing.T) {
	c := fromAPI(newMockddbAPI(t))
	err := c.Update(context.Background(), "Users", Item{"id": sKey("u-1")}, UpdateOpts{})
	if err == nil || !strings.Contains(err.Error(), "nothing to update") {
		t.Fatalf("expected nothing-to-update error, got %v", err)
	}
}

func TestClient_Delete_PropagatesError(t *testing.T) {
	api := newMockddbAPI(t)
	c := fromAPI(api)
	api.EXPECT().DeleteItem(mock.Anything, mock.Anything).
		Return(nil, errors.New("not authorized")).Once()
	err := c.Delete(context.Background(), "Users", Item{"id": sKey("u-1")})
	if err == nil || !strings.Contains(err.Error(), "not authorized") {
		t.Fatalf("expected not-authorized, got %v", err)
	}
}

func TestClient_Query_ReturnsItems(t *testing.T) {
	api := newMockddbAPI(t)
	c := fromAPI(api)
	api.EXPECT().Query(mock.Anything, mock.MatchedBy(func(in *dynamodb.QueryInput) bool {
		return awssdk.ToString(in.TableName) == "Orders"
	})).Return(&dynamodb.QueryOutput{
		Items: []Item{
			{"id": sKey("o-1")},
			{"id": sKey("o-2")},
		},
	}, nil).Once()

	items, err := c.Query(context.Background(), "Orders", QueryOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items", len(items))
	}
}

func TestClient_Scan_ReturnsItems(t *testing.T) {
	api := newMockddbAPI(t)
	c := fromAPI(api)
	api.EXPECT().Scan(mock.Anything, mock.Anything).Return(&dynamodb.ScanOutput{
		Items: []Item{{"id": sKey("a")}},
	}, nil).Once()
	items, err := c.Scan(context.Background(), "Orders", ScanOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item")
	}
}

func TestClient_PutTyped_MarshalsStruct(t *testing.T) {
	api := newMockddbAPI(t)
	c := fromAPI(api)

	var captured *dynamodb.PutItemInput
	api.EXPECT().PutItem(mock.Anything, mock.Anything).
		Run(func(ctx context.Context, in *dynamodb.PutItemInput, opts ...func(*dynamodb.Options)) {
			captured = in
		}).
		Return(&dynamodb.PutItemOutput{}, nil).Once()

	err := c.PutTyped(context.Background(), "Users", Person{ID: "p-1", Name: "alice", Age: 42})
	if err != nil {
		t.Fatal(err)
	}
	if captured.Item["id"].(*types.AttributeValueMemberS).Value != "p-1" {
		t.Fatalf("id not marshaled: %v", captured.Item["id"])
	}
	if captured.Item["age"].(*types.AttributeValueMemberN).Value != "42" {
		t.Fatalf("age not marshaled: %v", captured.Item["age"])
	}
}

func TestClient_GetTyped_HitAndMiss(t *testing.T) {
	api := newMockddbAPI(t)
	c := fromAPI(api)

	// Hit
	api.EXPECT().GetItem(mock.Anything, mock.Anything).Return(&dynamodb.GetItemOutput{
		Item: Item{
			"id":   sKey("p-1"),
			"name": sKey("alice"),
			"age":  &types.AttributeValueMemberN{Value: "30"},
		},
	}, nil).Once()
	var p Person
	ok, err := c.GetTyped(context.Background(), "Users", Item{"id": sKey("p-1")}, &p)
	if err != nil || !ok {
		t.Fatalf("expected hit, got ok=%v err=%v", ok, err)
	}
	if p != (Person{ID: "p-1", Name: "alice", Age: 30}) {
		t.Fatalf("unmarshal mismatch: %+v", p)
	}

	// Miss
	api.EXPECT().GetItem(mock.Anything, mock.Anything).Return(&dynamodb.GetItemOutput{}, nil).Once()
	var p2 Person
	ok, err = c.GetTyped(context.Background(), "Users", Item{"id": sKey("missing")}, &p2)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected miss")
	}
}

// === pure expression-builder tests (kept) ===


func TestBuildUpdateExpression_SetOnly(t *testing.T) {
	updates := Item{
		"name": &types.AttributeValueMemberS{Value: "alice"},
	}
	expr, names, vals := buildUpdateExpression(updates, nil)
	if !strings.HasPrefix(expr, "SET ") {
		t.Fatalf("expected SET prefix, got %q", expr)
	}
	if len(names) != 1 || len(vals) != 1 {
		t.Fatalf("names/vals: %d/%d", len(names), len(vals))
	}
	// Names map a placeholder back to the real attribute name.
	var found bool
	for _, v := range names {
		if v == "name" {
			found = true
		}
	}
	if !found {
		t.Fatal("missing name remap")
	}
}

func TestBuildUpdateExpression_RemoveOnly(t *testing.T) {
	expr, names, vals := buildUpdateExpression(nil, []string{"deprecated"})
	if !strings.HasPrefix(expr, "REMOVE ") {
		t.Fatalf("expected REMOVE prefix, got %q", expr)
	}
	if len(vals) != 0 {
		t.Fatal("expected no values for remove-only")
	}
	var found bool
	for _, v := range names {
		if v == "deprecated" {
			found = true
		}
	}
	if !found {
		t.Fatal("missing remove name")
	}
}

func TestBuildUpdateExpression_SetAndRemove(t *testing.T) {
	updates := Item{
		"name": &types.AttributeValueMemberS{Value: "bob"},
	}
	expr, _, _ := buildUpdateExpression(updates, []string{"old"})
	if !strings.Contains(expr, "SET ") || !strings.Contains(expr, "REMOVE ") {
		t.Fatalf("expected both clauses, got %q", expr)
	}
}

type Person struct {
	ID   string `dynamodbav:"id"`
	Name string `dynamodbav:"name"`
	Age  int    `dynamodbav:"age"`
}

func TestMarshalRoundTrip(t *testing.T) {
	in := Person{ID: "p1", Name: "alice", Age: 30}
	item, err := MarshalItem(in)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := item["id"]; !ok {
		t.Fatal("missing id attribute")
	}
	var out Person
	if err := UnmarshalItem(item, &out); err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("round-trip mismatch: %+v vs %+v", in, out)
	}
}

func TestUnmarshalItemsSlice(t *testing.T) {
	items := []Item{
		{"id": &types.AttributeValueMemberS{Value: "a"}, "name": &types.AttributeValueMemberS{Value: "alice"}, "age": &types.AttributeValueMemberN{Value: "1"}},
		{"id": &types.AttributeValueMemberS{Value: "b"}, "name": &types.AttributeValueMemberS{Value: "bob"}, "age": &types.AttributeValueMemberN{Value: "2"}},
	}
	var out []Person
	if err := UnmarshalItems(items, &out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[0].ID != "a" || out[1].ID != "b" {
		t.Fatalf("unexpected: %+v", out)
	}
}
