/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"flag"
	"log"
	"time"

	labv1alpha1 "github.com/ictsc/ictsc-kocto/api/v1alpha1"
	"github.com/ictsc/ictsc-kocto/internal/bundle"
)

func main() {
	var source labv1alpha1.ProblemSourceSpec
	flag.StringVar(&source.Repository, "repository", "", "Public GitHub repository")
	flag.StringVar(&source.CommitHash, "commit", "", "Exact commit SHA")
	flag.StringVar(&source.Directory, "directory", "", "Directory in repository")
	flag.StringVar(&source.TopologyFile, "topology", "", "Topology path relative to directory")
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	if err := bundle.Build(ctx, source, "/disk", "/opt/bundle/build-bundle.sh"); err != nil {
		log.Fatal(err)
	}
}
