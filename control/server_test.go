package control

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDashboardShowsEscapedCommit(t *testing.T) {
	server := &Server{commit: `abc12345<script>`}
	request := httptest.NewRequest("GET", "/dashboard", nil)
	response := httptest.NewRecorder()

	server.handleDashboard(response, request)

	body := response.Body.String()
	if !strings.Contains(body, `data-commit="abc12345&lt;script&gt;"`) {
		t.Fatalf("dashboard does not contain escaped commit: %q", body)
	}
	if strings.Contains(body, "{{BUILD_COMMIT}}") {
		t.Fatal("dashboard still contains the build commit placeholder")
	}
}
