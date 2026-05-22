package runtime

import "testing"

func TestCurrent_DefaultsToProductionWhenUnset(t *testing.T) {
	t.Setenv(envVar, "")
	if got := Current(); got != ModeProduction {
		t.Fatalf("unset → expected %q, got %q", ModeProduction, got)
	}
}

func TestCurrent_Dev(t *testing.T) {
	t.Setenv(envVar, "dev")
	if got := Current(); got != ModeDev {
		t.Fatalf("dev → expected %q, got %q", ModeDev, got)
	}
}

func TestCurrent_DevCaseInsensitive(t *testing.T) {
	t.Setenv(envVar, "DEV")
	if got := Current(); got != ModeDev {
		t.Fatalf("DEV → expected %q, got %q", ModeDev, got)
	}
}

func TestCurrent_DevelopmentAlias(t *testing.T) {
	t.Setenv(envVar, "development")
	if got := Current(); got != ModeDev {
		t.Fatalf("development → expected %q, got %q", ModeDev, got)
	}
}

func TestCurrent_Test(t *testing.T) {
	t.Setenv(envVar, "test")
	if got := Current(); got != ModeTest {
		t.Fatalf("test → expected %q, got %q", ModeTest, got)
	}
}

func TestCurrent_GarbageFailsClosed(t *testing.T) {
	t.Setenv(envVar, "yolo")
	if got := Current(); got != ModeProduction {
		t.Fatalf("garbage → expected %q (fail-closed), got %q", ModeProduction, got)
	}
}

func TestSetForTest_OverridesEnv(t *testing.T) {
	t.Setenv(envVar, "production")
	SetForTest(t, "dev")
	if !IsDev() {
		t.Fatalf("SetForTest(dev) should win over env=production")
	}
}

func TestSetForTest_RevertsOnCleanup(t *testing.T) {
	t.Setenv(envVar, "production")
	// Inner subtest sets override; outer test asserts that after subtest
	// finishes, override is gone and env wins again.
	t.Run("inner", func(t *testing.T) {
		SetForTest(t, "dev")
		if !IsDev() {
			t.Fatal("inner: expected dev")
		}
	})
	if IsDev() {
		t.Fatal("override leaked past subtest cleanup")
	}
}
