package t3loc

import (
	"encoding/json"
	"testing"
)

func TestLocator(t *testing.T) {
	for _, c := range []struct {
		id   string
		want string
	}{
		{`"a:b"`, "t3:th:x:a:b"},
		{`12`, "t3:th:x:12"},
		{`1.5e3`, "t3:th:x:1500"},
		{`0.25`, "t3:th:x:0.25"},
		{`null`, "t3:th:x:"},
		{`true`, "t3:th:x:true"},
	} {
		var v any
		if err := json.Unmarshal([]byte(c.id), &v); err != nil {
			t.Fatal(err)
		}
		if got := Locator("th:x", v); got != c.want {
			t.Fatalf("Locator(%s) = %q, want %q", c.id, got, c.want)
		}
	}
}
