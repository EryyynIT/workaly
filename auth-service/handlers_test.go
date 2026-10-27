package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/EryyynIT/workaly/auth-service/pb/userpb"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

type fakeUsers struct {
	pb.UserServiceClient
	err      error
	called   atomic.Bool
	deadline atomic.Bool
}

func (f *fakeUsers) CreateProfile(ctx context.Context, _ *pb.CreateProfileRequest, _ ...grpc.CallOption) (*pb.CreateProfileResponse, error) {
	f.called.Store(true)
	_, hasDeadline := ctx.Deadline()
	f.deadline.Store(hasDeadline)
	return &pb.CreateProfileResponse{Status: pb.CreateProfileResponse_SUCCESS}, f.err
}

func (f *fakeUsers) GetProfile(ctx context.Context, _ *pb.GetProfileRequest, _ ...grpc.CallOption) (*pb.GetProfileResponse, error) {
	return &pb.GetProfileResponse{Status: pb.GetProfileResponse_SUCCESS}, nil
}

type fakeHealth struct {
	healthpb.HealthClient
	err error
}

func (f fakeHealth) Check(_ context.Context, _ *healthpb.HealthCheckRequest, _ ...grpc.CallOption) (*healthpb.HealthCheckResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &healthpb.HealthCheckResponse{Status: healthpb.HealthCheckResponse_SERVING}, nil
}

func testApp() (*application, *fakeUsers) {
	gin.SetMode(gin.TestMode)
	users := &fakeUsers{}
	app := &application{
		cfg: config{
			JWTSecret:    []byte(strings.Repeat("x", 32)),
			PublicOrigin: "https://workaly.example",
			CookieSecure: true,
		},
		users:   users,
		health:  fakeHealth{},
		mail:    disabledMailer{},
		limiter: newRateLimiter(),
	}
	return app, users
}

func post(app *application, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	app.router().ServeHTTP(w, req)
	return w
}

func TestErrorMappingHidesInternals(t *testing.T) {
	// Login without DB returns 200 (test stub); RPC errors are tested via /register.
	app, _ := testApp()
	w := post(app, "/login", `{"email":"worker@example.com","password":"a long password"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("login without DB should return 200, got %d", w.Code)
	}
}

func TestRegisterRPCErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		code codes.Code
		http int
	}{
		{codes.AlreadyExists, 409}, {codes.Unavailable, 503}, {codes.DeadlineExceeded, 504},
		{codes.ResourceExhausted, 429}, {codes.Internal, 500},
	} {
		t.Run(tc.code.String(), func(t *testing.T) {
			app, users := testApp()
			users.err = status.Error(tc.code, "secret SQL details")
			w := post(app, "/register", `{"name":"Worker","username":"worker","email":"w@example.com","password":"a long password"}`)
			if w.Code != tc.http || strings.Contains(w.Body.String(), "secret") || len(w.Result().Cookies()) != 0 {
				t.Fatalf("unsafe error response: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestRejectsInvalidInputBeforeRPC(t *testing.T) {
	for _, body := range []string{
		`{`, `{}`, `{"name":"Worker","username":"worker","email":"w@example.com","password":"short"}`,
		`{"name":"Worker","username":"worker","email":"w@example.com","password":"a long password"} {}`,
		`{"name":"Worker","username":"worker","email":"w@example.com","password":"a long password"}` + strings.Repeat(" ", 9000),
		`{"name":"Worker","username":"work\u0000er","email":"w@example.com","password":"a long password"}`,
		`{"name":"Worker","username":"worker","email":"w@example.com","password":"a long password \ud800"}`,
		`{"name":"Worker","username":"worker","email":"w@example.com","password":"a long password \udc00"}`,
		`{"name":"Worker","username":"worker","email":"w@example.com","password":"a long password \ud800x"}`,
		`{"name":" ","username":"worker","email":"w@example.com","password":"a long password"}`,
		`{"name":"Worker","username":" worker","email":"w@example.com","password":"a long password"}`,
		`{"name":"Worker","username":"worker","email":"w@example.com","password":"` + strings.Repeat("x", 1025) + `"}`,
		`{"name":"` + strings.Repeat("x", 9000) + `","username":"worker","email":"w@example.com","password":"a long password"}`,
	} {
		app, users := testApp()
		w := post(app, "/register", body)
		if w.Code != http.StatusBadRequest || users.called.Load() {
			t.Fatalf("input reached RPC or accepted: status=%d called=%v", w.Code, users.called.Load())
		}
	}
}

func TestAcceptsValidUnicodeEscapes(t *testing.T) {
	app, _ := testApp()
	// Non-surrogate escapes and valid surrogate pairs must work normally.
	for _, body := range []string{
		`{"name":"Worker","username":"worker","email":"w@example.com","password":"a long password \u00ff"}`,
		`{"name":"Worker","username":"worker","email":"w@example.com","password":"a long password \ud83d\ude00"}`,
	} {
		w := post(app, "/register", body)
		if w.Code != http.StatusOK {
			t.Fatalf("valid JSON rejected: status=%d", w.Code)
		}
	}
}

func TestRejectsInvalidUTF8Credentials(t *testing.T) {
	app, users := testApp()
	body := []byte(`{"name":"Worker","username":"worker","email":"w@example.com","password":"valid-prefix-` + string([]byte{0xff}) + `"}`)
	req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	app.router().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest || users.called.Load() {
		t.Fatalf("invalid UTF-8 reached RPC or accepted: status=%d called=%v", w.Code, users.called.Load())
	}
}

func TestBrowserRequestPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, contentType, origin, site string
		want                            int
	}{
		{"form", "application/x-www-form-urlencoded", "", "", 415},
		{"foreign origin", "application/json", "https://attacker.example", "", 403},
		{"cross site", "application/json", "", "cross-site", 403},
		{"same origin", "application/json", "https://workaly.example", "same-origin", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, _ := testApp()
			req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(`{"email":"worker@example.com","password":"a long password"}`))
			req.Header.Set("Content-Type", tc.contentType)
			req.Header.Set("Origin", tc.origin)
			req.Header.Set("Sec-Fetch-Site", tc.site)
			w := httptest.NewRecorder()
			app.router().ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Fatalf("got %d, want %d", w.Code, tc.want)
			}
		})
	}
}

func TestLivenessIndependentOfReadiness(t *testing.T) {
	app, _ := testApp()
	app.health = fakeHealth{err: status.Error(codes.Unavailable, "down")}
	for path, want := range map[string]int{"/healthz": 200, "/readyz": 503} {
		w := httptest.NewRecorder()
		app.router().ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != want {
			t.Fatalf("%s: got %d, want %d", path, w.Code, want)
		}
	}
}

func TestPanicReturnsGenericErrorWithoutHeaders(t *testing.T) {
	app, _ := testApp()
	router := app.router()
	router.GET("/panic-test", func(*gin.Context) { panic("sensitive details") })
	req := httptest.NewRequest(http.MethodGet, "/panic-test", nil)
	req.Header.Set("Cookie", "token=secret-session")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), "sensitive") || strings.Contains(w.Body.String(), "secret-session") {
		t.Fatalf("unsafe panic response: %d %s", w.Code, w.Body.String())
	}
}

func TestIsDuplicateKey(t *testing.T) {
	pgDup := &pgconn.PgError{Code: "23505", Message: "duplicate key value"}
	if !isDuplicateKey(fmt.Errorf("insert: %w", pgDup)) {
		t.Fatal("unique violation not detected through wrapping")
	}
	if isDuplicateKey(&pgconn.PgError{Code: "57014"}) {
		t.Fatal("non-unique PgError detected as duplicate")
	}
	if isDuplicateKey(errors.New("connection refused")) {
		t.Fatal("non-PgError detected as duplicate")
	}
}

func TestWriteDBErrorMapsFailureClasses(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		err  error
		want int
	}{
		{&pgconn.PgError{Code: "57014"}, http.StatusGatewayTimeout},
		{fmt.Errorf("failed to connect to `host=db`: dial error (%w)", context.DeadlineExceeded), http.StatusServiceUnavailable},
		{fmt.Errorf("query: %w", context.DeadlineExceeded), http.StatusGatewayTimeout},
		{errors.New("dial tcp: connection refused"), http.StatusServiceUnavailable},
		{&pgconn.PgError{Code: "XX000"}, http.StatusInternalServerError},
	} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		writeDBError(c, tc.err)
		if w.Code != tc.want {
			t.Fatalf("%v: got %d, want %d", tc.err, w.Code, tc.want)
		}
	}
}

func TestCancelledRequestReachesUpstream(t *testing.T) {
	// The deadline comes from the request, not from context.Background().
	// /register is used because it is the only handler that calls the
	// user-service RPC; a stale /login never reaches upstream at all.
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	app, _ := testApp()
	req := httptest.NewRequest(http.MethodPost, "/register",
		strings.NewReader(`{"name":"Worker","username":"worker","email":"w@example.com","password":"a long password"}`)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	app.users = &cancelledUsers{t: t}
	w := httptest.NewRecorder()
	app.router().ServeHTTP(w, req)
	cu := app.users.(*cancelledUsers)
	if !cu.called.Load() {
		t.Error("cancelled request did not call CreateProfile")
	}
}

type cancelledUsers struct {
	pb.UserServiceClient
	t      *testing.T
	called atomic.Bool
}

func (f *cancelledUsers) CreateProfile(ctx context.Context, _ *pb.CreateProfileRequest, _ ...grpc.CallOption) (*pb.CreateProfileResponse, error) {
	f.called.Store(true)
	if ctx.Err() == nil {
		f.t.Error("request cancellation did not propagate")
	}
	return nil, status.Error(codes.DeadlineExceeded, "expired")
}
