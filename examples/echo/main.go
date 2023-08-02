// Copyright 2023 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"

	"github.com/ServiceWeaver/weaver"
)

//go:generate weaver generate

func main() {
	// Initialize the Service Weaver application.
	flag.Parse()
	if err := weaver.Run(context.Background(), serve); err != nil {
		log.Fatal(err)
	}
}

type server struct {
	weaver.Implements[weaver.Main]
	echoer weaver.Ref[Echoer]
	lis    weaver.Listener `weaver:"echo"`
}

func serve(ctx context.Context, s *server) error {
	// Setup the HTTP handler.
	var mux http.ServeMux
	mux.Handle("/", weaver.InstrumentHandlerFunc("echo",
		func(w http.ResponseWriter, r *http.Request) {
			input := r.URL.Query().Get("s")
			output, err := s.echoer.Get().Echo(r.Context(), input)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Write([]byte(output))
		}))
	mux.HandleFunc(weaver.HealthzURL, weaver.HealthzHandler)

	fmt.Printf("echo listener available on %v\n", s.lis)
	return http.Serve(s.lis, &mux)
}
