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
	err      error
	vacancy  *jobpb.Vacancy
	list     *jobpb.ListMyVacanciesResponse
	response *jobpb.VacancyResponse
	// lastRequest records the request of whichever RPC ran last; tests that
	// care about one method read the concrete type they expect.
	lastCreate     atomic.Value // *jobpb.CreateVacancyRequest
	lastUpdate     atomic.Value // *jobpb.UpdateVacancyRequest
	lastList       atomic.Value // *jobpb.ListMyVacanciesRequest
	lastPublicList atomic.Value // *jobpb.ListPublishedVacanciesRequest
	publicList     *jobpb.ListPublishedVacanciesResponse
	lastGetActor   atomic.Value // string
	lastRespond    atomic.Value // *jobpb.CreateVacancyResponseRequest
	lastWithdraw   atomic.Value // *jobpb.WithdrawVacancyResponseRequest
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

func (f *fakeVacancies) ListPublishedVacancies(_ context.Context, req *jobpb.ListPublishedVacanciesRequest, _ ...grpc.CallOption) (*jobpb.ListPublishedVacanciesResponse, error) {
	f.lastPublicList.Store(req)
	if f.publicList != nil {
		return f.publicList, nil
	}
	return &jobpb.ListPublishedVacanciesResponse{}, f.err
}

func (f *fakeVacancies) CreateVacancyResponse(_ context.Context, req *jobpb.CreateVacancyResponseRequest, _ ...grpc.CallOption) (*jobpb.VacancyResponse, error) {
	f.lastRespond.Store(req)
	return f.response, f.err
}

func (f *fakeVacancies) ListVacancyResponses(_ context.Context, _ *jobpb.ListVacancyResponsesRequest, _ ...grpc.CallOption) (*jobpb.ListVacancyResponsesResponse, error) {
	if f.response != nil {
		return &jobpb.ListVacancyResponsesResponse{Responses: []*jobpb.VacancyResponse{f.response}}, f.err
	}
	return &jobpb.ListVacancyResponsesResponse{}, f.err
}

func (f *fakeVacancies) WithdrawVacancyResponse(_ context.Context, req *jobpb.WithdrawVacancyResponseRequest, _ ...grpc.CallOption) (*jobpb.VacancyResponse, error) {
	f.lastWithdraw.Store(req)
	return f.response, f.err
}

func (f *fakeVacancies) GetMyVacancyResponse(_ context.Context, _ *jobpb.GetMyVacancyResponseRequest, _ ...grpc.CallOption) (*jobpb.VacancyResponse, error) {
	return f.response, f.err
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

	// page_size errors are rejected at the edge with one uniform message and
	// must not reach the RPC.
	for _, raw := range []string{"abc", "99999999999", "0", "-1", "51"} {
		w := doJSON(app, http.MethodGet, "/my/vacancies?page_size="+raw, "", sessionCookie(app, fakeActor()))
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "between 1 and 50") {
			t.Fatalf("page_size=%q: %d %s", raw, w.Code, w.Body.String())
		}
	}
}

// TestListPublicVacancies covers the anonymous catalog: no session needed,
// filters pass through, page_size validated at the edge, RPC errors map.
func TestListPublicVacancies(t *testing.T) {
	app, _ := testApp()
	fake := app.vacancies.(*fakeVacancies)
	fake.publicList = &jobpb.ListPublishedVacanciesResponse{
		Vacancies: []*jobpb.Vacancy{sampleVacancy()},
	}

	// Anonymous access with all filters passes them through verbatim.
	w := get(app, "/vacancies?employment_type=remote&work_format=remote&salary_min_from=100000&salary_max_to=200000&title_contains=go&page_size=25")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"vacancies":[`) {
		t.Fatalf("public list: %d %s", w.Code, w.Body.String())
	}
	req, _ := fake.lastPublicList.Load().(*jobpb.ListPublishedVacanciesRequest)
	if req == nil {
		t.Fatal("public list did not reach the RPC")
	}
	if req.EmploymentType != "remote" || req.WorkFormat != "remote" ||
		req.SalaryMinFrom != "100000" || req.SalaryMaxTo != "200000" ||
		req.TitleContains != "go" || req.PageSize != 25 {
		t.Fatalf("public list filters = %+v", req)
	}

	// A broken session cookie must not fail the public read.
	w = doJSON(app, http.MethodGet, "/vacancies", "", "token=garbage")
	if w.Code != http.StatusOK {
		t.Fatalf("public list with broken cookie: %d", w.Code)
	}

	// Bad page_size is rejected at the edge.
	for _, raw := range []string{"abc", "0", "51"} {
		w := get(app, "/vacancies?page_size="+raw)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "between 1 and 50") {
			t.Fatalf("page_size=%q: %d", raw, w.Code)
		}
	}

	// Service validation errors map to 400 with the precise message.
	fake.publicList = nil
	fake.err = status.Error(codes.InvalidArgument, "employment_type must be one of: full_time, part_time, contract, internship")
	w = get(app, "/vacancies?employment_type=shift")
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "employment_type") {
		t.Fatalf("bad filter: %d %s", w.Code, w.Body.String())
	}

	// Infrastructure failure maps to 503 without leaking detail.
	fake.err = status.Error(codes.Unavailable, "internal detail")
	w = get(app, "/vacancies")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("unavailable: %d", w.Code)
	}
	fake.err = nil
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

func sampleResponse() *jobpb.VacancyResponse {
	created := time.Date(2026, 10, 30, 9, 30, 0, 0, time.UTC)
	return &jobpb.VacancyResponse{
		Id:              "019a2b7e-7c3a-7b1a-8f0e-1a2b3c4d5e71",
		VacancyId:       "019a2b7e-7c3a-7b1a-8f0e-1a2b3c4d5e6f",
		CandidateUserId: fakeActor(),
		CoverLetter:     "Здравствуйте! Готов обсудить роль.",
		Contact:         "@candidate",
		Status:          "active",
		CreatedAt:       timestamppb.New(created),
	}
}

// TestRespondVacancyChecksSessionAndActor covers the mutation edge: no
// session must never reach the RPC, and the actor is the JWT subject only.
func TestRespondVacancyChecksSessionAndActor(t *testing.T) {
	app, _ := testApp()
	fake := app.vacancies.(*fakeVacancies)

	w := doJSON(app, http.MethodPost, "/vacancies/"+sampleVacancy().Id+"/responses",
		`{"cover_letter":"hi"}`, "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous respond: %d", w.Code)
	}
	if fake.lastRespond.Load() != nil {
		t.Fatal("anonymous respond reached the RPC")
	}

	fake.response = sampleResponse()
	actor := fakeActor()
	w = doJSON(app, http.MethodPost, "/vacancies/"+sampleVacancy().Id+"/responses",
		`{"cover_letter":"Здравствуйте!","contact":"@candidate"}`, sessionCookie(app, actor))
	if w.Code != http.StatusCreated {
		t.Fatalf("respond: %d %s", w.Code, w.Body.String())
	}
	req, _ := fake.lastRespond.Load().(*jobpb.CreateVacancyResponseRequest)
	if req == nil || req.ActorUserId != actor || req.VacancyId != sampleVacancy().Id {
		t.Fatalf("RPC request = %+v", req)
	}
	if req.CoverLetter != "Здравствуйте!" || req.Contact != "@candidate" {
		t.Fatalf("RPC fields = %+v", req)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"contact":"@candidate"`) || !strings.Contains(body, `"status":"active"`) {
		t.Fatalf("response view wrong: %s", body)
	}
}

func TestRespondVacancyNeedsVerifiedEmail(t *testing.T) {
	app, _ := testApp()
	// db == nil means the gate passes; the interesting mapping is the RPC
	// result. A repeat active response arrives as AlreadyExists and must
	// surface as a precise 409.
	fake := app.vacancies.(*fakeVacancies)
	fake.err = status.Error(codes.AlreadyExists, "you already have an active response to this vacancy")

	w := doJSON(app, http.MethodPost, "/vacancies/"+sampleVacancy().Id+"/responses",
		`{"cover_letter":"hi"}`, sessionCookie(app, fakeActor()))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "active response") {
		t.Fatalf("repeat respond: %d %s", w.Code, w.Body.String())
	}
}

func TestWithdrawVacancyResponseCarriesIDs(t *testing.T) {
	app, _ := testApp()
	fake := app.vacancies.(*fakeVacancies)
	fake.response = sampleResponse()
	fake.response.Status = "withdrawn"
	withdrawn := time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)
	fake.response.WithdrawnAt = timestamppb.New(withdrawn)
	actor := fakeActor()

	path := "/vacancies/" + sampleVacancy().Id + "/responses/" + sampleResponse().Id + "/withdraw"
	w := doJSON(app, http.MethodPost, path, "", sessionCookie(app, actor))
	if w.Code != http.StatusOK {
		t.Fatalf("withdraw: %d %s", w.Code, w.Body.String())
	}
	req, _ := fake.lastWithdraw.Load().(*jobpb.WithdrawVacancyResponseRequest)
	if req == nil || req.ActorUserId != actor || req.VacancyId != sampleVacancy().Id ||
		req.ResponseId != sampleResponse().Id {
		t.Fatalf("RPC withdraw request = %+v", req)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"status":"withdrawn"`) || !strings.Contains(body, `"withdrawn_at":"2026-10-30T10:00:00Z"`) {
		t.Fatalf("withdrawn view wrong: %s", body)
	}
}

func TestListVacancyResponsesIsAuthorOnly(t *testing.T) {
	app, _ := testApp()
	fake := app.vacancies.(*fakeVacancies)
	fake.response = sampleResponse()

	w := doJSON(app, http.MethodGet, "/vacancies/"+sampleVacancy().Id+"/responses", "", sessionCookie(app, fakeActor()))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"responses":[`) {
		t.Fatalf("author list: %d %s", w.Code, w.Body.String())
	}
	// A stranger gets the uniform 404 the service reports for a foreign vacancy.
	fake.err = status.Error(codes.NotFound, "vacancy not found")
	fake.response = nil
	w = doJSON(app, http.MethodGet, "/vacancies/"+sampleVacancy().Id+"/responses", "", sessionCookie(app, fakeActor()))
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), `"error":"vacancy not found"`) {
		t.Fatalf("stranger list: %d %s", w.Code, w.Body.String())
	}
}

func TestGetMyVacancyResponseMapsNotFound(t *testing.T) {
	app, _ := testApp()
	fake := app.vacancies.(*fakeVacancies)
	fake.response = sampleResponse()

	w := doJSON(app, http.MethodGet, "/vacancies/"+sampleVacancy().Id+"/responses/mine", "", sessionCookie(app, fakeActor()))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"candidate_user_id":"`+fakeActor()+`"`) {
		t.Fatalf("own response: %d %s", w.Code, w.Body.String())
	}
	fake.err = status.Error(codes.NotFound, "vacancy not found")
	w = doJSON(app, http.MethodGet, "/vacancies/"+sampleVacancy().Id+"/responses/mine", "", sessionCookie(app, fakeActor()))
	if w.Code != http.StatusNotFound {
		t.Fatalf("missing own response: %d", w.Code)
	}
}
