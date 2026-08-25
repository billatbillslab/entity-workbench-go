package entitysdk

// Direct-call probe for channelInboxHandler.Handle.
//
// The cross-impl F-CIMP-2 finding observed `status=500 invalid entity:
// data is empty` on every burst delivery against wb-go. The throughput
// test never reads the handler's return status — events flow through
// the channel before the response is constructed — so the 500 is only
// observed on the *dispatching* peer's logs. This direct call bypasses
// the wire, asserts the response status returned for a valid
// notification, and discriminates between "handler is fine, framework
// drops Params.Data under concurrent inbound" and "handler's own
// success path is broken".

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.entitychurch.org/entity-core-go/core/ecf"
	"go.entitychurch.org/entity-core-go/core/entity"
	"go.entitychurch.org/entity-core-go/core/handler"
	"go.entitychurch.org/entity-core-go/core/types"
)

func mkValidReceiveRequest(t *testing.T) *handler.Request {
	t.Helper()
	notif := types.SubscriptionNotificationData{
		URI:   "entity://peerX/watched/0000001",
		Event: "put",
	}
	raw, err := ecf.Encode(notif)
	if err != nil {
		t.Fatalf("encode notification: %v", err)
	}
	ent, err := entity.NewEntity("system/subscription/notification", raw)
	if err != nil {
		t.Fatalf("construct notification entity: %v", err)
	}
	return &handler.Request{
		Path:      "system/inbox/sdk-sub-test",
		Operation: "receive",
		Params:    ent,
	}
}

func decodeErrorBody(resp *handler.Response) string {
	if resp == nil || len(resp.Result.Data) == 0 {
		return ""
	}
	var errData types.ErrorData
	if err := ecf.Decode(resp.Result.Data, &errData); err != nil {
		return "decode-error:" + err.Error()
	}
	return "code=" + errData.Code + " message=" + errData.Message
}

func TestChannelInboxHandler_DirectCall_ValidNotification(t *testing.T) {
	events := make(chan ChangeEvent, 16)
	h := &channelInboxHandler{out: events}

	go func() {
		select {
		case <-events:
		case <-time.After(time.Second):
		}
	}()

	resp, err := h.Handle(context.Background(), mkValidReceiveRequest(t))
	if err != nil {
		t.Fatalf("Handle returned err: %v", err)
	}
	if resp == nil {
		t.Fatal("nil response")
	}
	t.Logf("response: status=%d result.type=%q result.data_len=%d body=%q",
		resp.Status, resp.Result.Type, len(resp.Result.Data), decodeErrorBody(resp))
	if resp.Status != 200 {
		t.Fatalf("expected status=200 for valid notification, got %d (%s)",
			resp.Status, decodeErrorBody(resp))
	}
}

// Burst variant: invokes Handle K times concurrently with valid input,
// captures every response. Confirms whether the failure correlates with
// goroutine concurrency on a single handler instance or fires
// deterministically on every call.
func TestChannelInboxHandler_DirectCall_BurstK16(t *testing.T) {
	const K = 16
	events := make(chan ChangeEvent, K*2)
	h := &channelInboxHandler{out: events}

	// Drain events so out-chan writes don't block.
	go func() {
		for range events {
		}
	}()

	var ok200, ok500, okOther, errCount atomic.Int64
	var sampleMsg atomic.Value // string

	var wg sync.WaitGroup
	for i := 0; i < K; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := h.Handle(context.Background(), mkValidReceiveRequest(t))
			if err != nil {
				errCount.Add(1)
				return
			}
			switch resp.Status {
			case 200:
				ok200.Add(1)
			case 500:
				ok500.Add(1)
				if msg := decodeErrorBody(resp); msg != "" {
					sampleMsg.Store(msg)
				}
			default:
				okOther.Add(1)
			}
		}()
	}
	wg.Wait()
	close(events)

	msg, _ := sampleMsg.Load().(string)
	t.Logf("K=%d direct calls: status200=%d status500=%d other=%d err=%d sample_msg=%q",
		K, ok200.Load(), ok500.Load(), okOther.Load(), errCount.Load(), msg)

	if ok200.Load() == 0 {
		t.Errorf("zero 200 responses — handler's success path is broken (sample message: %q)", msg)
	}
}
