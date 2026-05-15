package handler

import "testing"

func TestKnowledgeIndexHealthDetectsReadyEmptyCollection(t *testing.T) {
	t.Parallel()

	status := knowledgeIndexHealthStatus(1, 0, nil)
	if !status.Unhealthy {
		t.Fatal("expected ready source with zero indexed points to be unhealthy")
	}
	if status.ErrorMessage == "" {
		t.Fatal("expected resync error message")
	}
}

func TestKnowledgeIndexHealthIgnoresEmptyCollectionWithoutReadySources(t *testing.T) {
	t.Parallel()

	status := knowledgeIndexHealthStatus(0, 0, nil)
	if status.Unhealthy {
		t.Fatalf("expected no ready sources to be healthy, got %+v", status)
	}
}
