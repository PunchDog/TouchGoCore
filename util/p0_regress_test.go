package util

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestRandInt_InRange(t *testing.T) {
	for i := 0; i < 100000; i++ {
		if v := RandInt(10); v < 0 || v >= 10 {
			t.Fatalf("RandInt(10) = %d, 超出 [0,10)", v)
		}
	}
}

func TestRandInt_ZeroAndNegativeMax(t *testing.T) {
	if RandInt(0) != 0 {
		t.Fatal("RandInt(0) 应为 0")
	}
	if RandInt(-5) != 0 {
		t.Fatal("RandInt(-5) 应为 0，不得 panic（npc 空 slice 依赖此语义）")
	}
}

func TestRandInt_Concurrent(t *testing.T) {
	var wg sync.WaitGroup
	for g := 0; g < 64; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("并发 RandInt panic: %v", r)
				}
			}()
			for i := 0; i < 10000; i++ {
				if v := RandInt(3); v < 0 || v >= 3 {
					t.Errorf("RandInt(3) = %d 越界", v)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestRandRange(t *testing.T) {
	for i := 0; i < 10000; i++ {
		if v := RandRange(10, 5); v < 5 || v >= 10 {
			t.Fatalf("RandRange(10,5) = %d 超出 [5,10)", v)
		}
		if v := RandRange(5, 10); v < 5 || v >= 10 {
			t.Fatalf("RandRange(5,10)（参数倒置）= %d 超出 [5,10)", v)
		}
	}
	if v := RandRange(7, 7); v != 7 {
		t.Fatalf("RandRange(7,7) = %d, 应为 7", v)
	}
}

func TestPostMultipartForm_NonOKReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	body, err := PostMultipartForm([]MultipartFormField{{IsFile: false, Fieldname: "f", Value: []byte("v")}}, srv.URL)
	if err == nil {
		t.Fatalf("非 200 响应必须返回错误，实际 err=nil body=%q", body)
	}
	if body != nil {
		t.Fatalf("非 200 响应 body 应为 nil")
	}
}

func TestPostMultipartForm_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	body, err := PostMultipartForm([]MultipartFormField{{IsFile: false, Fieldname: "f", Value: []byte("v")}}, srv.URL)
	if err != nil || string(body) != "ok" {
		t.Fatalf("200 响应应成功: body=%q err=%v", body, err)
	}
}
