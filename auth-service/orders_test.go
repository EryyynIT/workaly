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
	deal     *taskpb.Deal
	// lastCreate records the request of whichever RPC ran last.
	lastCreate     atomic.Value // *taskpb.CreateOrderRequest
	lastPropose    atomic.Value // *taskpb.CreateOrderProposalRequest
	lastPublicList atomic.Value // *taskpb.ListPublishedOrdersRequest
	lastDealCall   atomic.Value // the request of the last deal pipeline RPC
	publicList     *taskpb.ListPublishedOrdersResponse
	proposalList   *taskpb.ListOrderProposalsResponse
	proposalPages  []*taskpb.ListOrderProposalsResponse
	proposalPage   int
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
	if f.proposalPages != nil {
		page := f.proposalPage
		if page >= len(f.proposalPages) {
			return &taskpb.ListOrderProposalsResponse{}, f.err
		}
		f.proposalPage++
		return f.proposalPages[page], f.err
	}
	if f.proposalList != nil {
		return f.proposalList, nil
	}
	return &taskpb.ListOrderProposalsResponse{}, f.err
}

func (f *fakeOrders) WithdrawOrderProposal(_ context.Context, _ *taskpb.WithdrawOrderProposalRequest, _ ...grpc.CallOption) (*taskpb.OrderProposal, error) {
	return f.proposal, f.err
}

func (f *fakeOrders) GetMyOrderProposal(_ context.Context, _ *taskpb.GetMyOrderProposalRequest, _ ...grpc.CallOption) (*taskpb.OrderProposal, error) {
	return f.proposal, f.err
}

func (f *fakeOrders) SelectProposal(_ context.Context, req *taskpb.SelectProposalRequest, _ ...grpc.CallOption) (*taskpb.Deal, error) {
	f.lastDealCall.Store(req)
	return f.deal, f.err
}

func (f *fakeOrders) ConfirmDealTerms(_ context.Context, req *taskpb.ConfirmDealTermsRequest, _ ...grpc.CallOption) (*taskpb.Deal, error) {
	f.lastDealCall.Store(req)
	return f.deal, f.err
}

func (f *fakeOrders) DeclineDealTerms(_ context.Context, req *taskpb.DeclineDealTermsRequest, _ ...grpc.CallOption) (*taskpb.Deal, error) {
	f.lastDealCall.Store(req)
	return f.deal, f.err
}

func (f *fakeOrders) SubmitResult(_ context.Context, req *taskpb.SubmitResultRequest, _ ...grpc.CallOption) (*taskpb.Deal, error) {
	f.lastDealCall.Store(req)
	return f.deal, f.err
}

func (f *fakeOrders) AcceptResult(_ context.Context, req *taskpb.AcceptResultRequest, _ ...grpc.CallOption) (*taskpb.Deal, error) {
	f.lastDealCall.Store(req)
	return f.deal, f.err
}

func (f *fakeOrders) RequestRevision(_ context.Context, req *taskpb.RequestRevisionRequest, _ ...grpc.CallOption) (*taskpb.Deal, error) {
	f.lastDealCall.Store(req)
	return f.deal, f.err
}

func (f *fakeOrders) CancelDeal(_ context.Context, req *taskpb.CancelDealRequest, _ ...grpc.CallOption) (*taskpb.Deal, error) {
	f.lastDealCall.Store(req)
	return f.deal, f.err
}

func (f *fakeOrders) GetDeal(_ context.Context, _ *taskpb.GetDealRequest, _ ...grpc.CallOption) (*taskpb.Deal, error) {
	return f.deal, f.err
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

// TestDealPipelineEdge covers the S3.2 deal at the HTTP edge: the actor and
// ids reach the RPCs verbatim, selection terms default to the proposal's
// ones, and the shared error mapping covers conflicts and races.
func TestDealPipelineEdge(t *testing.T) {
	oid := sampleOrder().Id
	pid := "019a2b7e-7c3a-7b1a-8f0e-1a2b3c4d5e99"
	actor := fakeActor()
	sample := sampleDeal(oid, actor)

	t.Run("deal calls require a session", func(t *testing.T) {
		app, _ := testApp()
		fake := app.orders.(*fakeOrders)
		for _, item := range []struct {
			method, path, body string
		}{
			{http.MethodPost, "/orders/" + oid + "/proposals/" + pid + "/select", `{}`},
			{http.MethodGet, "/orders/" + oid + "/deal", ""},
			{http.MethodPost, "/orders/" + oid + "/deal/confirm", ""},
			{http.MethodPost, "/orders/" + oid + "/deal/decline", `{}`},
			{http.MethodPost, "/orders/" + oid + "/deal/submit", `{"result":"готово","idempotency_key":"table-submit-key"}`},
			{http.MethodPost, "/orders/" + oid + "/deal/accept", ""},
			{http.MethodPost, "/orders/" + oid + "/deal/revision", `{"reason":"доработать"}`},
			{http.MethodPost, "/orders/" + oid + "/deal/cancel", `{"reason":"передумал"}`},
		} {
			w := doJSON(app, item.method, item.path, item.body, "")
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("anonymous %s %s: %d", item.method, item.path, w.Code)
			}
		}
		if fake.lastDealCall.Load() != nil {
			t.Fatal("anonymous call reached the RPC")
		}
	})

	t.Run("changed revision terms require executor handshake", func(t *testing.T) {
		app, _ := testApp()
		fake := app.orders.(*fakeOrders)
		pending := sampleDeal(oid, actor)
		pending.Status = "terms_pending"
		pending.StartedAt = nil
		fake.deal = pending
		fake.proposalList = &taskpb.ListOrderProposalsResponse{Proposals: []*taskpb.OrderProposal{{
			Id: pid, ProposedAmount: "90000.00", Currency: "RUB", ProposedDeadline: "2026-11-30",
		}}}
		w := doJSON(app, http.MethodPost, "/orders/"+oid+"/proposals/"+pid+"/select",
			`{"revisions_included":2}`, sessionCookie(app, actor))
		if w.Code != http.StatusCreated || !strings.Contains(w.Body.String(), `"status":"terms_pending"`) {
			t.Fatalf("changed revision terms must await executor: %d %s", w.Code, w.Body.String())
		}
		req, _ := fake.lastDealCall.Load().(*taskpb.SelectProposalRequest)
		if req == nil || req.RevisionsIncluded != 2 {
			t.Fatalf("custom revision terms lost: %+v", req)
		}
	})

	t.Run("select defaults to the proposal terms", func(t *testing.T) {
		app, _ := testApp()
		fake := app.orders.(*fakeOrders)
		fake.deal = sample
		fake.proposalList = &taskpb.ListOrderProposalsResponse{Proposals: []*taskpb.OrderProposal{{
			Id:               pid,
			OrderId:          oid,
			ExecutorUserId:   sample.ExecutorUserId,
			ProposedAmount:   "90000.00",
			Currency:         "RUB",
			ProposedDeadline: "2026-11-30",
			Status:           "active",
		}}}

		w := doJSON(app, http.MethodPost, "/orders/"+oid+"/proposals/"+pid+"/select",
			`{}`, sessionCookie(app, actor))
		if w.Code != http.StatusCreated {
			t.Fatalf("select: %d %s", w.Code, w.Body.String())
		}
		req, _ := fake.lastDealCall.Load().(*taskpb.SelectProposalRequest)
		if req == nil || req.ActorUserId != actor || req.OrderId != oid || req.ProposalId != pid {
			t.Fatalf("RPC select request = %+v", req)
		}
		if req.Amount != "90000.00" || req.Currency != "RUB" || req.Deadline != "2026-11-30" {
			t.Fatalf("default terms must mirror the proposal: %+v", req)
		}
	})

	t.Run("select rejects a repeated page cursor", func(t *testing.T) {
		app, _ := testApp()
		fake := app.orders.(*fakeOrders)
		fake.proposalPages = []*taskpb.ListOrderProposalsResponse{
			{NextPageToken: "same-cursor"},
			{NextPageToken: "same-cursor"},
		}
		w := doJSON(app, http.MethodPost, "/orders/"+oid+"/proposals/"+pid+"/select", `{}`, sessionCookie(app, actor))
		if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "invalid page cursor") {
			t.Fatalf("repeated cursor: %d %s", w.Code, w.Body.String())
		}
		if fake.proposalPage != 2 {
			t.Fatalf("expected to stop at repeated cursor, called %d pages", fake.proposalPage)
		}
	})

	t.Run("select default terms searches later proposal pages", func(t *testing.T) {
		app, _ := testApp()
		fake := app.orders.(*fakeOrders)
		fake.deal = sample
		fake.proposalPages = []*taskpb.ListOrderProposalsResponse{
			{Proposals: []*taskpb.OrderProposal{{Id: "older-proposal"}}, NextPageToken: "next-page"},
			{Proposals: []*taskpb.OrderProposal{{
				Id:               pid,
				ProposedAmount:   "90000.00",
				Currency:         "RUB",
				ProposedDeadline: "2026-11-30",
			}}},
		}
		w := doJSON(app, http.MethodPost, "/orders/"+oid+"/proposals/"+pid+"/select", `{}`, sessionCookie(app, actor))
		if w.Code != http.StatusCreated || fake.proposalPage != 2 {
			t.Fatalf("select later page: status=%d pages=%d body=%s", w.Code, fake.proposalPage, w.Body.String())
		}
		req, _ := fake.lastDealCall.Load().(*taskpb.SelectProposalRequest)
		if req == nil || req.Amount != "90000.00" || req.Currency != "RUB" {
			t.Fatalf("proposal terms not forwarded: %+v", req)
		}
	})

	t.Run("select forwards custom terms verbatim", func(t *testing.T) {
		app, _ := testApp()
		fake := app.orders.(*fakeOrders)
		fake.deal = sample
		fake.proposalList = &taskpb.ListOrderProposalsResponse{Proposals: []*taskpb.OrderProposal{{
			Id: pid, ProposedAmount: "90000.00", Currency: "RUB", ProposedDeadline: "2026-11-30",
		}}}
		w := doJSON(app, http.MethodPost, "/orders/"+oid+"/proposals/"+pid+"/select",
			`{"amount":"85000","currency":"RUB","deadline":"2026-12-15","revisions_included":2,"acceptance_criteria":"тесты зелёные"}`,
			sessionCookie(app, actor))
		if w.Code != http.StatusCreated {
			t.Fatalf("select with terms: %d %s", w.Code, w.Body.String())
		}
		req, _ := fake.lastDealCall.Load().(*taskpb.SelectProposalRequest)
		if req == nil || req.Amount != "85000" || req.RevisionsIncluded != 2 ||
			req.AcceptanceCriteria != "тесты зелёные" {
			t.Fatalf("custom terms lost: %+v", req)
		}
		if fake.proposalList != nil && fake.lastDealCall.Load() == nil {
			t.Fatal("custom terms must not read the proposals list")
		}
	})

	t.Run("select requires amount and currency together", func(t *testing.T) {
		app, _ := testApp()
		fake := app.orders.(*fakeOrders)
		fake.deal = sample
		w := doJSON(app, http.MethodPost, "/orders/"+oid+"/proposals/"+pid+"/select",
			`{"amount":"85000"}`, sessionCookie(app, actor))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("partial custom terms: %d %s", w.Code, w.Body.String())
		}
		if fake.lastDealCall.Load() != nil {
			t.Fatal("incomplete custom terms reached the RPC")
		}
	})

	t.Run("transitions pass actor and order id", func(t *testing.T) {
		app, _ := testApp()
		fake := app.orders.(*fakeOrders)
		fake.deal = sample

		w := doJSON(app, http.MethodPost, "/orders/"+oid+"/deal/submit",
			`{"result":"первая итерация","idempotency_key":"stable-submit-1"}`, sessionCookie(app, actor))
		if w.Code != http.StatusOK {
			t.Fatalf("submit: %d %s", w.Code, w.Body.String())
		}
		req, _ := fake.lastDealCall.Load().(*taskpb.SubmitResultRequest)
		if req == nil || req.ActorUserId != actor || req.OrderId != oid || req.Result != "первая итерация" || req.IdempotencyKey != "stable-submit-1" {
			t.Fatalf("RPC submit request = %+v", req)
		}
	})

	t.Run("submit requires idempotency key", func(t *testing.T) {
		app, _ := testApp()
		fake := app.orders.(*fakeOrders)
		w := doJSON(app, http.MethodPost, "/orders/"+oid+"/deal/submit",
			`{"result":"первая итерация"}`, sessionCookie(app, actor))
		if w.Code != http.StatusBadRequest || fake.lastDealCall.Load() != nil {
			t.Fatalf("missing key should be rejected before RPC: %d %s", w.Code, w.Body.String())
		}
	})

	t.Run("revision carries the reason", func(t *testing.T) {
		app, _ := testApp()
		fake := app.orders.(*fakeOrders)
		fake.deal = sample
		w := doJSON(app, http.MethodPost, "/orders/"+oid+"/deal/revision",
			`{"reason":"поправить шапку"}`, sessionCookie(app, actor))
		if w.Code != http.StatusOK {
			t.Fatalf("revision: %d %s", w.Code, w.Body.String())
		}
		rev, _ := fake.lastDealCall.Load().(*taskpb.RequestRevisionRequest)
		if rev == nil || rev.Reason != "поправить шапку" {
			t.Fatalf("RPC revision request = %+v", rev)
		}
	})

	t.Run("cancel carries the reason", func(t *testing.T) {
		app, _ := testApp()
		fake := app.orders.(*fakeOrders)
		fake.deal = sample
		w := doJSON(app, http.MethodPost, "/orders/"+oid+"/deal/cancel",
			`{"reason":"обстоятельства изменились"}`, sessionCookie(app, actor))
		if w.Code != http.StatusOK {
			t.Fatalf("cancel: %d %s", w.Code, w.Body.String())
		}
		can, _ := fake.lastDealCall.Load().(*taskpb.CancelDealRequest)
		if can == nil || can.Reason != "обстоятельства изменились" {
			t.Fatalf("RPC cancel request = %+v", can)
		}
	})

	t.Run("deal view carries terms, submissions and timestamps", func(t *testing.T) {
		app, _ := testApp()
		fake := app.orders.(*fakeOrders)
		full := sampleDeal(oid, actor)
		full.Status = "submitted"
		full.Submissions = []*taskpb.DealSubmission{{
			Iteration:      1,
			Result:         "первая итерация",
			SubmittedAt:    full.CreatedAt,
			RevisionReason: "поправить шапку",
		}}
		fake.deal = full
		w := doJSON(app, http.MethodGet, "/orders/"+oid+"/deal", "", sessionCookie(app, actor))
		if w.Code != http.StatusOK {
			t.Fatalf("get deal: %d %s", w.Code, w.Body.String())
		}
		body := w.Body.String()
		for _, fragment := range []string{
			`"status":"submitted"`, `"amount":"90000.00"`, `"revisions_included":2`,
			`"revision_reason":"поправить шапку"`, `"submissions"`,
		} {
			if !strings.Contains(body, fragment) {
				t.Fatalf("deal view misses %s: %s", fragment, body)
			}
		}
	})

	t.Run("state conflicts and races map to 409", func(t *testing.T) {
		app, _ := testApp()
		fake := app.orders.(*fakeOrders)
		fake.err = status.Error(codes.FailedPrecondition, "deal is submitted; this transition requires in_progress")
		w := doJSON(app, http.MethodPost, "/orders/"+oid+"/deal/submit",
			`{"result":"ещё раз","idempotency_key":"race-submit-key"}`, sessionCookie(app, actor))
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "requires in_progress") {
			t.Fatalf("conflict: %d %s", w.Code, w.Body.String())
		}
	})

	t.Run("selection race maps to 409", func(t *testing.T) {
		app, _ := testApp()
		fake := app.orders.(*fakeOrders)
		fake.err = status.Error(codes.AlreadyExists, "this order already has a deal")
		w := doJSON(app, http.MethodPost, "/orders/"+oid+"/proposals/"+pid+"/select",
			`{"amount":"90000","currency":"RUB"}`, sessionCookie(app, actor))
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "already has a deal") {
			t.Fatalf("selection race: %d %s", w.Code, w.Body.String())
		}
	})

	t.Run("a missing or foreign deal is the uniform 404", func(t *testing.T) {
		app, _ := testApp()
		fake := app.orders.(*fakeOrders)
		fake.err = status.Error(codes.NotFound, "order not found")
		w := doJSON(app, http.MethodGet, "/orders/"+oid+"/deal", "", sessionCookie(app, actor))
		if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), `"error":"order not found"`) {
			t.Fatalf("undisclosing 404: %d %s", w.Code, w.Body.String())
		}
	})
}

func sampleDeal(orderID, customerID string) *taskpb.Deal {
	created := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	return &taskpb.Deal{
		Id:             "019a2b7e-7c3a-7b1a-8f0e-1a2b3c4d5e98",
		OrderId:        orderID,
		CustomerUserId: customerID,
		ExecutorUserId: "019a2b7e-7c3a-7b1a-8f0e-1a2b3c4d5e70",
		Status:         "in_progress",
		Terms: &taskpb.DealTerms{
			Amount:            "90000.00",
			Currency:          "RUB",
			Deadline:          "2026-11-30",
			RevisionsIncluded: 2,
		},
		Version:   1,
		CreatedAt: timestamppb.New(created),
		StartedAt: timestamppb.New(created),
	}
}
