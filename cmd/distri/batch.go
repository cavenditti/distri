package main

import (
	"context"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"path/filepath"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/stapelberg/zi/internal/env"
	"github.com/stapelberg/zi/pb"
	"golang.org/x/sync/errgroup"
	"gonum.org/v1/gonum/graph"
	"gonum.org/v1/gonum/graph/simple"
	"gonum.org/v1/gonum/graph/topo"
)

// batch builder.
// use cases:
// - continuous build system:
//   + a new commit comes in, mirror should be updated
//   + local modification to a local tree, rebuild all affected packages
//
// - build-all (e.g. create distri image)

// milestone: -bootstrap flag: print which packages need to be bootstrapped (those which depend on themselves)
//   needs cycle detection (e.g. pkg-config→glib→pkg-config)
// milestone: run a single build action
// milestone: run multiple build actions in parallel
// milestone: run a build action on a remote machine using cpu(1)

// build action
// - input: source tarball, build.textproto, build dep images
// - output: image

// figure out what needs to be built:
// check if the outputs are present

// to rebuild the archive: increment version number of all packages (helper tool which does this and commits?)

const batchHelp = `TODO
`

type node struct {
	id   int64
	name string
}

func (n *node) ID() int64 { return n.id }

func batch(args []string) error {
	fset := flag.NewFlagSet("batch", flag.ExitOnError)
	fset.Parse(args)

	log.Printf("distriroot %q", env.DistriRoot)

	// TODO: use simple.NewDirectedMatrix instead?
	g := simple.NewDirectedGraph()

	pkgsDir := filepath.Join(env.DistriRoot, "pkgs")
	fis, err := ioutil.ReadDir(pkgsDir)
	if err != nil {
		return err
	}
	byName := make(map[string]*node)
	for idx, fi := range fis {
		pkg := fi.Name()

		// TODO(later): parallelize?
		c, err := ioutil.ReadFile(filepath.Join(pkgsDir, fi.Name(), "build.textproto"))
		if err != nil {
			return err
		}
		var buildProto pb.Build
		if err := proto.UnmarshalText(string(c), &buildProto); err != nil {
			return err
		}

		// TODO: to conserve work, only add nodes which need to be rebuilt
		n := &node{id: int64(idx), name: pkg + "-" + buildProto.GetVersion()}
		byName[n.name] = n
		g.AddNode(n)
	}

	// add all constraints: <pkg>-<version> depends on <pkg>-<version>
	for _, fi := range fis {
		pkg := fi.Name()

		// TODO(later): parallelize?
		c, err := ioutil.ReadFile(filepath.Join(pkgsDir, fi.Name(), "build.textproto"))
		if err != nil {
			return err
		}
		var buildProto pb.Build
		if err := proto.UnmarshalText(string(c), &buildProto); err != nil {
			return err
		}
		version := buildProto.GetVersion()

		// TODO: use just builder deps + GetDep + GetRuntimeDep
		//buildDeps, err := builddeps(&buildProto)
		deps := buildProto.GetDep()
		deps = append(deps, builderdeps(&buildProto)...)
		deps = append(deps, buildProto.GetRuntimeDep()...)

		n := byName[pkg+"-"+version]
		for _, dep := range deps {
			if dep == pkg+"-"+version {
				continue // TODO
			}
			g.SetEdge(g.NewEdge(n, byName[dep]))
		}
	}

	// detect cycles and break them

	// strong-set = packages which needed a cycle break
	// 1. build the strong-set once, in any order, with host deps (= remove all deps)
	// 2. build the strong-set again, with the results of the previous compilation
	// 3. build the rest of the packages

	// scc := topo.TarjanSCC(g)
	// log.Printf("%d scc", len(scc))
	// for idx, c := range scc {
	// 	log.Printf("scc %d", idx)
	// 	for _, n := range c {
	// 		log.Printf("  n %v", n)
	// 	}
	// }

	// Break cycles
	if _, err := topo.Sort(g); err != nil {
		uo, ok := err.(topo.Unorderable)
		if !ok {
			return err
		}
		for _, component := range uo { // cyclic component
			//log.Printf("uo %d", idx)
			for _, n := range component {
				//log.Printf("  n %v", n)
				from := g.From(n.ID())
				for from.Next() {
					g.RemoveEdge(n.ID(), from.Node().ID())
				}
			}
		}
		if _, err := topo.Sort(g); err != nil {
			return fmt.Errorf("could not break cycles: %v", err)
		}
	}

	s := scheduler{
		g:      g,
		byName: byName,
		built:  make(map[string]bool),
	}
	if err := s.run(); err != nil {
		return err
	}

	return nil
}

type buildResult struct {
	name    string
	success bool
}

type scheduler struct {
	g      graph.Directed
	byName map[string]*node
	built  map[string]bool
}

func (s *scheduler) run() error {
	numNodes := s.g.Nodes().Len()
	work := make(chan string, numNodes)
	done := make(chan buildResult)
	eg, ctx := errgroup.WithContext(context.Background())
	for i := 0; i < 8; i++ {
		eg.Go(func() error {
			for pkg := range work {
				dur := 10*time.Millisecond + time.Duration(rand.Int63n(int64(100*time.Millisecond)))
				log.Printf("build of %s is taking %v", pkg, dur)
				time.Sleep(dur)
				done <- buildResult{name: pkg, success: pkg != "libx11-1.6.6"}
			}
			return nil
		})
	}

	// Enqueue all packages which have no dependencies to get the build started:
	for nodes := s.g.Nodes(); nodes.Next(); {
		n := nodes.Node()
		if s.g.From(n.ID()).Len() == 0 {
			work <- n.(*node).name
		}
	}
	go func() {
		defer close(work)
		for len(s.built) < numNodes { // scheduler tick
			select {
			case result := <-done:
				log.Printf("build %s completed", result.name)
				n := s.byName[result.name]
				s.built[result.name] = result.success
				if !result.success {
					s.markFailed(n)
				} else {
					for to := s.g.To(n.ID()); to.Next(); {
						if candidate := to.Node(); s.canBuild(candidate) {
							log.Printf("  → enqueuing %s", candidate.(*node).name)
							work <- candidate.(*node).name
						}
					}
				}

			case <-ctx.Done():
				return
			}
		}
	}()
	if err := eg.Wait(); err != nil {
		return err
	}
	succeeded := 0
	for _, result := range s.built {
		if result {
			succeeded++
		}
	}

	log.Printf("%d packages succeeded, %d failed, %d total", succeeded, len(s.built)-succeeded, len(s.built))

	return nil
}

func (s *scheduler) markFailed(n graph.Node) {
	log.Printf("marking deps of %s as failed", n.(*node).name)
	for to := s.g.To(n.ID()); to.Next(); {
		d := to.Node()
		log.Printf("→ %s failed", d.(*node).name)
		if s.built[d.(*node).name] {
			log.Fatalf("BUG: %s already succeeded, but dependencies cannot be fulfilled", d.(*node).name)
		}
		s.built[d.(*node).name] = false // dependencies cannot be fulfilled
		s.markFailed(d)
	}
}

// canBuild returns whether all dependencies of candidate are built.
func (s *scheduler) canBuild(candidate graph.Node) bool {
	//log.Printf("  checking %s", candidate.(*node).name)
	for from := s.g.From(candidate.ID()); from.Next(); {
		if name := from.Node().(*node).name; !s.built[name] {
			//log.Printf("  dep %s not yet ready", name)
			return false
		}
	}
	return true

}

// bison needs help2man <stage1>
// help2man needs perl
// perl needs glibc
// glibc needs bison
