package main

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xheize/git-updater/internal/controller"
	"github.com/xheize/git-updater/internal/gitManager"
)

type fakeController struct {
	called int
	err    error
}

func (f *fakeController) Repository(context.Context) (gitManager.RepositoryView, error) {
	f.called++
	return gitManager.RepositoryView{}, f.err
}
func (f *fakeController) Preview(context.Context, controller.Intent) (controller.Plan, error) {
	f.called++
	return controller.Plan{State: "planned"}, f.err
}
func (f *fakeController) ApplyPlan(context.Context, string) (controller.Plan, error) {
	f.called++
	return controller.Plan{State: "published"}, f.err
}
func TestControllerAPIAuthenticationAndStatus(t *testing.T) {
	app, store, _ := newContractAPI(t)
	engine := &fakeController{}
	setupControllerRoutes(app, engine, store, "contract-key")
	for _, tc := range []struct{ method, path string }{{"GET", "/api/repository"}, {"GET", "/api/jobs"}, {"GET", "/api/changesets"}, {"GET", "/api/changesets/x"}, {"POST", "/api/changesets"}, {"POST", "/api/changesets/x/apply"}} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader("{}"))
		res, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 401 {
			t.Fatalf("%s auth bypassed: %d", tc.path, res.StatusCode)
		}
	}
	if engine.called != 0 {
		t.Fatal("unauthenticated controller call")
	}
	for _, tc := range []struct {
		err  error
		want int
	}{{nil, 200}, {controller.ErrInvalid, 422}, {controller.ErrConflict, 409}, {controller.ErrNotFound, 404}} {
		engine.err = tc.err
		contractRequest(t, app, "POST", "/api/changesets/x/apply", "", contractAuth(), tc.want)
	}
	contractRequest(t, app, "GET", "/api/changesets?limit=101", "", contractAuth(), 400)
	contractRequest(t, app, "GET", "/api/jobs?offset=-1", "", contractAuth(), 400)
	contractRequest(t, app, "GET", "/api/changesets", "", contractAuth(), 200)
}
