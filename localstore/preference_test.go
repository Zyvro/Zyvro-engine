package localstore

import "testing"

func TestTheOrderAndTheEndpointsDoNotEraseEachOther(t *testing.T) {
	// They share one file, which is the point — both answer "which backend does
	// this project use" — but it means either writer can drop the other's half,
	// and the symptom would be a local server that silently disconnects the
	// moment somebody reorders a list.
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetProviderEndpoint("lmstudio", Endpoint{URL: "http://127.0.0.1:1234/v1", Model: "qwen"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetProviderOrder(map[string][]string{"vision": {"lmstudio", "google"}}); err != nil {
		t.Fatal(err)
	}
	if got := s.ProviderEndpoints()["lmstudio"].Model; got != "qwen" {
		t.Errorf("the endpoint was lost when the order was saved: %q", got)
	}
	if err := s.SetProviderEndpoint("custom", Endpoint{URL: "http://127.0.0.1:9000/v1"}); err != nil {
		t.Fatal(err)
	}
	if got := s.ProviderOrder()["vision"]; len(got) != 2 {
		t.Errorf("the order was lost when an endpoint was saved: %v", got)
	}
}

func TestAnEmptyEndpointIsForgottenRatherThanStoredBlank(t *testing.T) {
	// A blank entry would still count as "turned on", which is exactly the
	// state somebody clearing the fields is trying to leave.
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_ = s.SetProviderEndpoint("custom", Endpoint{URL: "http://127.0.0.1:9000/v1"})
	if err := s.SetProviderEndpoint("custom", Endpoint{}); err != nil {
		t.Fatal(err)
	}
	if _, still := s.ProviderEndpoints()["custom"]; still {
		t.Error("the cleared endpoint is still in the file")
	}
}
