package workflows

import "context"

// TestReadNodeOutput is a test-only accessor. Production callers
// should consume node outputs inside their downstream handlers via
// the parents map; this helper exists so e2e tests can verify the
// final output of a terminal node after the workflow completes.
//
// Returns ErrNotFound if no output is stored for that node.
func TestReadNodeOutput(e *Engine, instanceID, nodeName string) ([]byte, error) {
	val, ok, err := e.opts.Outputs.Get(context.Background(), nodeOutputKey(e.opts.KeyPrefix, instanceID, nodeName))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotFound
	}
	return val, nil
}
