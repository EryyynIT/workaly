package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	taskpb "github.com/EryyynIT/workaly/auth-service/pb/taskpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type fakeOrders struct {
	taskpb.TaskServiceClient
	err      error
	order    *taskpb.Order
	proposal *taskpb.OrderProposal
	// lastCreate records the request of whichever RPC ran last.
	lastCreate     atomic.Value // *taskpb.CreateOrderRequest
	lastPropose    atomic.Value // *taskpb.CreateOrderProposalRequest
	lastPublicList atomic.Value // *taskpb.ListPublishedOrdersRequest
	publicList     *taskpb.ListPublishedOrdersResponse
}

func (f *fakeOrders) CreateOrder(_ context.Context, req *taskpb.CreateOrderRequest, _ ...grpc.CallOption) (*taskpb.Order, error) {
	f.lastCreate.Store(req)
	return f.order, f.err
}

func (f *fakeOrders) GetOrder(_ context.Context, _ *taskpb.GetOrderRequest, _ ...grpc.CallOption) (*taskpb.Order, error) {
	return f.order, f.err
}

func (f *fakeOrders) UpdateOrder(_ context.Context, _ *taskpb.UpdateOrderRequest, _ ...grpc.CallOption) (*taskpb.Order, error) {
	return f.order, f.err
}

func (f *fakeOrders) PublishOrder(_ context.Context, _ *taskpb.PublishOrderRequest, _ ...grpc.CallOption) (*taskpb.Order, error) {
	return f.order, f.err
}

func (f *fakeOrders) CloseOrder(_ context.Context, _ *taskpb.CloseOrderRequest, _ ...grpc.CallOption) (*taskpb.Order, error) {
	return f.order, f.err
}

func (f *fakeOrders) DeleteDraft(_ context.Context, _ *taskpb.DeleteDraftRequest, _ ...grpc.CallOption) (*taskpb.Order, error) {
	return f.order, f.err
}

func (f *fakeOrders) ListMyOrders(_ context.Context, _ *taskpb.ListMyOrdersRequest, _ ...grpc.CallOption) (*taskpb.ListMyOrdersResponse, error) {
	return &taskpb.ListMyOrdersResponse{}, f.err
}

func (f *fakeOrders) ListPublishedOrders(_ context.Context, req *taskpb.ListPublishedOrdersRequest, _ ...grpc.CallOption) (*taskpb.ListPublishedOrdersResponse, error) {
	f.lastPublicList.Store(req)
	if f.publicList != nil {
		return f.publicList, nil
	}
	return &taskpb.ListPublishedOrdersResponse{}, f.err
}

func (f *fakeOrders) CreateOrderProposal(_ context.Context, req *taskpb.CreateOrderProposalRequest, _ ...grpc.CallOption) (*taskpb.OrderProposal, error) {
	f.lastPropose.Store(req)
	return f.proposal, f.err
}

func (f *fakeOrders) ListOrderProposals(_ context.Context, _ *taskpb.ListOrderProposalsRequest, _ ...grpc.CallOption) (*taskpb.ListOrderProposalsResponse, error) {
	return &taskpb.ListOrderProposalsResponse{}, f.err
}

func (f *fakeOrders) WithdrawOrderProposal(_ context.Context, _ *taskpb.WithdrawOrderProposalRequest, _ ...grpc.CallOption) (*taskpb.OrderProposal, error) {
	return f.proposal, f.err
}

func (f *fakeOrders) GetMyOrderProposal(_ context.Context, _ *taskpb.GetMyOrderProposalRequest, _ ...grpc.CallOption) (*taskpb.OrderProposal, error) {
	return f.proposal, f.err
}

func sampleOrder() *taskpb.Order {
	created := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	return &taskpb.Order{
		Id:           "019a2b7e-7c3a-7b1a-8f0e-1a2b3c4d5e6f",
		AuthorUserId: fakeActor(),
		Title:        "Лендинг для кофейни",
		Description:  "Одностраничный сайт с формой заказа.",
		BudgetType:   "fixed",
		BudgetAmount: "120000.00",
		Currency:     "RUB",
		Deadline:     "2026-12-31",
		Status:       "draft",
		Version:      1,
		CreatedAt:    timestamppb.New(created),
		UpdatedAt:    timestamppb.New(created),
	}
}

func orderBody() string {
	return `{"title":"Лендинг для кофейни","description":"Одностраничный сайт с формой заказа.",` +
		`"budget_type":"fixed","budget_amount":"120000","currency":"RUB","deadline":"2026-12-31"}`
}

func TestOrderRequiresSession(t *testing.T) {
	app, _ := testApp()
	fake := app.orders.(*fakeOrders)

	w := doJSON(app, http.MethodPost, "/orders", orderBody(), "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous create: %d", w.Code)
	}
	if fake.lastCreate.Load() != nil {
		t.Fatal("anonymous create reached the RPC")
	}
}

func TestCreateOrderReturnsViewAndActor(t *testing.T) {
	app, _ := testApp()
	fake := app.orders.(*fakeOrders)
	fake.order = sampleOrder()
	actor := fakeActor()

	w := doJSON(app, http.MethodPost, "/orders", orderBody(), sessionCookie(app, actor))
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	req, _ := fake.lastCreate.Load().(*taskpb.CreateOrderRequest)
	if req == nil || req.ActorUserId != actor || req.BudgetAmount != "120000" || req.Deadline != "2026-12-31" {
		t.Fatalf("RPC request = %+v", req)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"budget_amount":"120000.00"`) || !strings.Contains(body, `"deadline":"2026-12-31"`) {
		t.Fatalf("order view wrong: %s", body)
	}
	if !strings.Contains(body, `"status":"draft"`) || !strings.Contains(body, `"published_at":null`) {
		t.Fatalf("draft view wrong: %s", body)
	}
}

func TestOrderErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		code     codes.Code
		rpcMsg   string
		want     int
		fragment string
	}{
		{codes.NotFound, "internal detail", http.StatusNotFound, `"error":"order not found"`},
		{codes.InvalidArgument, "budget_amount must be greater than zero", http.StatusBadRequest, `"error":"budget_amount must be greater than zero"`},
		{codes.FailedPrecondition, "order changed since you last read it (current version 2, sent 1)", http.StatusConflict, `"error":"order changed since you last read it`},
		{codes.AlreadyExists, "you already have an active proposal for this order", http.StatusConflict, `"error":"you already have an active proposal`},
		{codes.Unavailable, "internal detail", http.StatusServiceUnavailable, `"error":"order service unavailable"`},
		{codes.DeadlineExceeded, "internal detail", http.StatusGatewayTimeout, `"error":"operation timed out`},
		{codes.Internal, "secret SQL details", http.StatusInternalServerError, `"error":"internal service error"`},
	} {
		t.Run(tc.code.String(), func(t *testing.T) {
			app, _ := testApp()
			fake := app.orders.(*fakeOrders)
			fake.err = status.Error(tc.code, tc.rpcMsg)
			w := doJSON(app, http.MethodPatch, "/orders/not-a-uuid", `{"version":1}`, sessionCookie(app, fakeActor()))
			if w.Code != tc.want || !strings.Contains(w.Body.String(), tc.fragment) {
				t.Fatalf("%s: %d %s", tc.code, w.Code, w.Body.String())
			}
			if tc.code == codes.Internal && strings.Contains(w.Body.String(), "secret") {
				t.Fatalf("internal error leaked detail: %s", w.Body.String())
			}
		})
	}
}

func TestProposeOrderChecksSessionActorAndRepeat(t *testing.T) {
	app, _ := testApp()
	fake := app.orders.(*fakeOrders)

	w := doJSON(app, http.MethodPost, "/orders/"+sampleOrder().Id+"/proposals",
		`{"message":"hi"}`, "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous propose: %d", w.Code)
	}
	if fake.lastPropose.Load() != nil {
		t.Fatal("anonymous propose reached the RPC")
	}

	fake.proposal = &taskpb.OrderProposal{
		Id:             "019a2b7e-7c3a-7b1a-8f0e-1a2b3c4d5e72",
		OrderId:        sampleOrder().Id,
		ExecutorUserId: fakeActor(),
		Message:        "Сделаю за неделю.",
		ProposedAmount: "90000.00",
		Currency:       "RUB",
		Status:         "active",
		CreatedAt:      timestamppb.New(time.Date(2026, 10, 30, 13, 0, 0, 0, time.UTC)),
	}
	actor := fakeActor()
	w = doJSON(app, http.MethodPost, "/orders/"+sampleOrder().Id+"/proposals",
		`{"message":"Сделаю за неделю.","proposed_amount":"90000","currency":"RUB","proposed_deadline":"2026-11-15"}`,
		sessionCookie(app, actor))
	if w.Code != http.StatusCreated {
		t.Fatalf("propose: %d %s", w.Code, w.Body.String())
	}
	req, _ := fake.lastPropose.Load().(*taskpb.CreateOrderProposalRequest)
	if req == nil || req.ActorUserId != actor || req.ProposedAmount != "90000" || req.ProposedDeadline != "2026-11-15" {
		t.Fatalf("RPC propose request = %+v", req)
	}
	if !strings.Contains(w.Body.String(), `"proposed_amount":"90000.00"`) {
		t.Fatalf("proposal view wrong: %s", w.Body.String())
	}

	// A repeat active proposal arrives as AlreadyExists and surfaces as 409.
	fake.err = status.Error(codes.AlreadyExists, "you already have an active proposal for this order")
	w = doJSON(app, http.MethodPost, "/orders/"+sampleOrder().Id+"/proposals",
		`{"message":"hi","proposed_amount":"1","currency":"RUB"}`, sessionCookie(app, actor))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "active proposal") {
		t.Fatalf("repeat propose: %d %s", w.Code, w.Body.String())
	}
}

func TestOrderMutationsRejectCrossSite(t *testing.T) {
	app, _ := testApp()
	cookie := sessionCookie(app, fakeActor())
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/orders"},
		{http.MethodPatch, "/orders/" + sampleOrder().Id},
		{http.MethodDelete, "/orders/" + sampleOrder().Id},
		{http.MethodPost, "/orders/" + sampleOrder().Id + "/publish"},
		{http.MethodPost, "/orders/" + sampleOrder().Id + "/proposals"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(orderBody()))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Cookie", cookie)
		req.Header.Set("Origin", "https://attacker.example")
		w := httptest.NewRecorder()
		app.router().ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s %s from foreign origin: %d", tc.method, tc.path, w.Code)
		}
	}
}

// TestListPublicOrders covers the anonymous catalog: no session needed,
// filters pass through, page_size validated at the edge, RPC errors map.
func TestListPublicOrders(t *testing.T) {
	app, _ := testApp()
	fake := app.orders.(*fakeOrders)
	fake.publicList = &taskpb.ListPublishedOrdersResponse{
		Orders: []*taskpb.Order{sampleOrder()},
	}

	// Anonymous access with all filters passes them through verbatim.
	w := get(app, "/orders?budget_type=fixed&budget_min=50000&budget_max=150000&title_contains=site&page_size=25")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"orders":[`) {
		t.Fatalf("public list: %d %s", w.Code, w.Body.String())
	}
	req, _ := fake.lastPublicList.Load().(*taskpb.ListPublishedOrdersRequest)
	if req == nil {
		t.Fatal("public list did not reach the RPC")
	}
	if req.BudgetType != "fixed" || req.BudgetMin != "50000" ||
		req.BudgetMax != "150000" || req.TitleContains != "site" || req.PageSize != 25 {
		t.Fatalf("public list filters = %+v", req)
	}

	// A broken session cookie must not fail the public read.
	w = doJSON(app, http.MethodGet, "/orders", "", "token=garbage")
	if w.Code != http.StatusOK {
		t.Fatalf("public list with broken cookie: %d", w.Code)
	}

	// Bad page_size is rejected at the edge.
	for _, raw := range []string{"abc", "0", "51"} {
		w := get(app, "/orders?page_size="+raw)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "between 1 and 50") {
			t.Fatalf("page_size=%q: %d", raw, w.Code)
		}
	}

	// Service validation errors map to 400 with the precise message.
	fake.publicList = nil
	fake.err = status.Error(codes.InvalidArgument, "budget_type must be one of: fixed, hourly")
	w = get(app, "/orders?budget_type=daily")
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "budget_type") {
		t.Fatalf("bad filter: %d %s", w.Code, w.Body.String())
	}

	// Infrastructure failure maps to 503 without leaking detail.
	fake.err = status.Error(codes.Unavailable, "internal detail")
	w = get(app, "/orders")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("unavailable: %d", w.Code)
	}
	fake.err = nil
}
