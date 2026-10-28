package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	jobpb "github.com/EryyynIT/workaly/auth-service/pb/jobpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type fakeVacancies struct {
	jobpb.JobServiceClient
	err     error
	vacancy *jobpb.Vacancy
	list    *jobpb.ListMyVacanciesResponse
	// lastRequest records the request of whichever RPC ran last; tests that
	// care about one method read the concrete type they expect.
	lastCreate   atomic.Value // *jobpb.CreateVacancyRequest
	lastUpdate   atomic.Value // *jobpb.UpdateVacancyRequest
	lastList     atomic.Value // *jobpb.ListMyVacanciesRequest
	lastGetActor atomic.Value // string
}

func (f *fakeVacancies) CreateVacancy(_ context.Context, req *jobpb.CreateVacancyRequest, _ ...grpc.CallOption) (*jobpb.Vacancy, error) {
	f.lastCreate.Store(req)
	return f.vacancy, f.err
}

func (f *fakeVacancies) GetVacancy(_ context.Context, req *jobpb.GetVacancyRequest, _ ...grpc.CallOption) (*jobpb.Vacancy, error) {
	f.lastGetActor.Store(req.ActorUserId)
	return f.vacancy, f.err
}

func (f *fakeVacancies) UpdateVacancy(_ context.Context, req *jobpb.UpdateVacancyRequest, _ ...grpc.CallOption) (*jobpb.Vacancy, error) {
	f.lastUpdate.Store(req)
	return f.vacancy, f.err
}

func (f *fakeVacancies) PublishVacancy(_ context.Context, _ *jobpb.PublishVacancyRequest, _ ...grpc.CallOption) (*jobpb.Vacancy, error) {
	return f.vacancy, f.err
}

func (f *fakeVacancies) CloseVacancy(_ context.Context, _ *jobpb.CloseVacancyRequest, _ ...grpc.CallOption) (*jobpb.Vacancy, error) {
	return f.vacancy, f.err
}

func (f *fakeVacancies) DeleteDraft(_ context.Context, _ *jobpb.DeleteDraftRequest, _ ...grpc.CallOption) (*jobpb.Vacancy, error) {
	return f.vacancy, f.err
}

func (f *fakeVacancies) ListMyVacancies(_ context.Context, req *jobpb.ListMyVacanciesRequest, _ ...grpc.CallOption) (*jobpb.ListMyVacanciesResponse, error) {
	f.lastList.Store(req)
	if f.list != nil {
		return f.list, nil
	}
	return &jobpb.ListMyVacanciesResponse{}, f.err
}

func sampleVacancy() *jobpb.Vacancy {
	created := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	return &jobpb.Vacancy{
		Id:             "019a2b7e-7c3a-7b1a-8f0e-1a2b3c4d5e6f",
		AuthorUserId:   "019a2b7e-7c3a-7b1a-8f0e-1a2b3c4d5e70",
		Title:          "Backend-разработчик",
		Description:    "Платформа обработки платежей.",
		EmploymentType: "full_time",
		WorkFormat:     "remote",
		SalaryMin:      "150000.00",
		Currency:       "RUB",
		Status:         "draft",
		Version:        1,
		CreatedAt:      timestamppb.New(created),
		UpdatedAt:      timestamppb.New(created),
	}
}

func vacancyBody() string {
	return `{"title":"Backend-разработчик","description":"Платформа обработки платежей.",` +
		`"employment_type":"full_time","work_format":"remote",` +
		`"salary_min":"150000","salary_max":"250000.50","currency":"RUB"}`
}

// doJSON issues a request with a JSON content type, an optional body and an
// optional session cookie.
func doJSON(app *application, method, path, body, cookie string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	w := httptest.NewRecorder()
	app.router().ServeHTTP(w, req)
	return w
}

func TestVacancyRequiresSession(t *testing.T) {
	app, _ := testApp()
	fake := app.vacancies.(*fakeVacancies)

	w := doJSON(app, http.MethodPost, "/vacancies", vacancyBody(), "")
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "authentication required") {
		t.Fatalf("anonymous create: %d %s", w.Code, w.Body.String())
	}
	if fake.lastCreate.Load() != nil {
		t.Fatal("anonymous create reached the RPC")
	}
}

func TestCreateVacancyReturnsViewAndActor(t *testing.T) {
	app, _ := testApp()
	fake := app.vacancies.(*fakeVacancies)
	fake.vacancy = sampleVacancy()
	actor := "019a2b7e-7c3a-7b1a-8f0e-1a2b3c4d5e70"

	w := doJSON(app, http.MethodPost, "/vacancies", vacancyBody(), sessionCookie(app, actor))
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	req, _ := fake.lastCreate.Load().(*jobpb.CreateVacancyRequest)
	if req == nil || req.ActorUserId != actor {
		t.Fatalf("RPC actor = %+v", req)
	}
	if req.SalaryMin != "150000" || req.SalaryMax != "250000.50" || req.Currency != "RUB" {
		t.Fatalf("RPC money fields = %q %q %q", req.SalaryMin, req.SalaryMax, req.Currency)
	}
	if req.Location != "" {
		t.Fatalf("RPC location = %q, want empty for absent field", req.Location)
	}
	// The response view: optionals are null, timestamps RFC 3339 UTC.
	body := w.Body.String()
	if !strings.Contains(body, `"status":"draft"`) || !strings.Contains(body, `"version":1`) {
		t.Fatalf("missing status/version: %s", body)
	}
	if !strings.Contains(body, `"salary_min":"150000.00"`) || !strings.Contains(body, `"salary_max":null`) {
		t.Fatalf("money fields wrong: %s", body)
	}
	if !strings.Contains(body, `"created_at":"2026-10-29T12:00:00Z"`) {
		t.Fatalf("timestamp format wrong: %s", body)
	}
	if !strings.Contains(body, `"published_at":null`) {
		t.Fatalf("draft must have published_at null: %s", body)
	}
}

func TestVacancyErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		code     codes.Code
		rpcMsg   string
		want     int
		fragment string
	}{
		{codes.NotFound, "internal detail", http.StatusNotFound, `"error":"vacancy not found"`},
		{codes.InvalidArgument, "title must contain 1..120 characters", http.StatusBadRequest, `"error":"title must contain 1..120 characters"`},
		{codes.FailedPrecondition, "vacancy changed since you last read it (current version 2, sent 1)", http.StatusConflict, `"error":"vacancy changed since you last read it`},
		{codes.Unavailable, "internal detail", http.StatusServiceUnavailable, `"error":"vacancy service unavailable"`},
		{codes.DeadlineExceeded, "internal detail", http.StatusGatewayTimeout, `"error":"operation timed out`},
		{codes.Internal, "secret SQL details", http.StatusInternalServerError, `"error":"internal service error"`},
	} {
		t.Run(tc.code.String(), func(t *testing.T) {
			app, _ := testApp()
			fake := app.vacancies.(*fakeVacancies)
			fake.err = status.Error(tc.code, tc.rpcMsg)
			w := doJSON(app, http.MethodPatch, "/vacancies/not-a-uuid", `{"version":1}`, sessionCookie(app, fakeActor()))
			if w.Code != tc.want || !strings.Contains(w.Body.String(), tc.fragment) {
				t.Fatalf("%s: %d %s", tc.code, w.Code, w.Body.String())
			}
			// Infrastructure messages must never leak upstream detail.
			if tc.code == codes.Internal && strings.Contains(w.Body.String(), "secret") {
				t.Fatalf("internal error leaked detail: %s", w.Body.String())
			}
		})
	}
}

func TestGetVacancyDegradesToAnonymous(t *testing.T) {
	app, _ := testApp()
	fake := app.vacancies.(*fakeVacancies)
	fake.vacancy = sampleVacancy()
	fake.vacancy.Status = "published"

	// No cookie: the read still works and the actor arrives empty.
	w := get(app, "/vacancies/"+sampleVacancy().Id)
	if w.Code != http.StatusOK {
		t.Fatalf("public read: %d %s", w.Code, w.Body.String())
	}
	if actor, _ := fake.lastGetActor.Load().(string); actor != "" {
		t.Fatalf("anonymous read sent actor %q", actor)
	}

	// A broken cookie must not change the public behavior.
	w = doJSON(app, http.MethodGet, "/vacancies/"+sampleVacancy().Id, "", "token=garbage")
	if w.Code != http.StatusOK {
		t.Fatalf("garbage cookie on public read: %d", w.Code)
	}
}

func TestPublishGateNeedsVerifiedEmail(t *testing.T) {
	app, _ := testApp()
	// db == nil (unit tests) means the gate passes; publishing reaches the RPC.
	fake := app.vacancies.(*fakeVacancies)
	fake.vacancy = sampleVacancy()
	fake.vacancy.Status = "published"

	w := doJSON(app, http.MethodPost, "/vacancies/"+sampleVacancy().Id+"/publish", "",
		sessionCookie(app, fakeActor()))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"status":"published"`) {
		t.Fatalf("publish with stubbed verification: %d %s", w.Code, w.Body.String())
	}
}

func TestPatchVacancyCarriesVersion(t *testing.T) {
	app, _ := testApp()
	fake := app.vacancies.(*fakeVacancies)
	fake.vacancy = sampleVacancy()
	actor := fakeActor()

	w := doJSON(app, http.MethodPatch, "/vacancies/"+sampleVacancy().Id, `{"version":3,"title":"Новое название","description":"Новое описание","employment_type":"contract","work_format":"onsite"}`, sessionCookie(app, actor))
	if w.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", w.Code, w.Body.String())
	}
	req, _ := fake.lastUpdate.Load().(*jobpb.UpdateVacancyRequest)
	if req == nil || req.Version != 3 || req.Title != "Новое название" || req.EmploymentType != "contract" {
		t.Fatalf("RPC update request = %+v", req)
	}
	if req.ActorUserId != actor {
		t.Fatalf("RPC actor = %q", req.ActorUserId)
	}
}

func TestListMyVacanciesMapsAndValidates(t *testing.T) {
	app, _ := testApp()
	fake := app.vacancies.(*fakeVacancies)
	fake.list = &jobpb.ListMyVacanciesResponse{Vacancies: []*jobpb.Vacancy{sampleVacancy()}}

	w := doJSON(app, http.MethodGet, "/my/vacancies", "", sessionCookie(app, fakeActor()))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"vacancies":[`) {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	req, _ := fake.lastList.Load().(*jobpb.ListMyVacanciesRequest)
	if req == nil || req.PageSize != 0 || req.PageToken != "" {
		t.Fatalf("defaults wrong: %+v", req)
	}

	// page_size parse errors must not reach the RPC; range checks live in the
	// service (a negative size is int32-valid and gets a precise 400 there).
	for _, raw := range []string{"abc", "99999999999"} {
		w := doJSON(app, http.MethodGet, "/my/vacancies?page_size="+raw, "", sessionCookie(app, fakeActor()))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("page_size=%q: %d", raw, w.Code)
		}
	}
}

func TestVacancyMutationsRejectCrossSite(t *testing.T) {
	app, _ := testApp()
	cookie := sessionCookie(app, fakeActor())
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/vacancies"},
		{http.MethodPatch, "/vacancies/" + sampleVacancy().Id},
		{http.MethodDelete, "/vacancies/" + sampleVacancy().Id},
		{http.MethodPost, "/vacancies/" + sampleVacancy().Id + "/publish"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(vacancyBody()))
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

func fakeActor() string {
	return "019a2b7e-7c3a-7b1a-8f0e-1a2b3c4d5e70"
}
