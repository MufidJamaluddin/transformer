package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"os"
	"os/signal"
	"runtime/pprof"
	"syscall"
	"time"

	"net/http"
	_ "net/http/pprof"

	"github.com/pkg/errors"
	G "gorgonia.org/gorgonia"
	"gorgonia.org/gorgonia/examples/mnist"
	"gorgonia.org/tensor"
	// "github.com/sugarme/sermo/example/gorgonia/mnist/mnist"

	bp "gopkg.in/cheggaaa/pb.v1"
)

var (
	epochs     = flag.Int("epochs", 10, "Number of epochs to train for")
	dataset    = flag.String("dataset", "train", "Which dataset to train on? Valid options are \"train\" or \"test\"")
	dtype      = flag.String("dtype", "float64", "Which dtype to use")
	batchsize  = flag.Int("batchsize", 100, "Batch size")
	cpuprofile = flag.String("cpuprofile", "", "CPU profiling")
)

const loc = "../testdata"

var dt tensor.Dtype

func parseDtype() {
	switch *dtype {
	case "float64":
		dt = tensor.Float64
	case "float32":
		dt = tensor.Float32
	default:
		log.Fatalf("Unknow dtype: %v", *dtype)
	}
}

type sli struct {
	start, end int
}

func (s sli) Start() int { return s.start }
func (s sli) End() int   { return s.end }
func (s sli) Step() int  { return 1 }

type convnet struct {
	g                  *G.ExprGraph
	w0, w1, w2, w3, w4 *G.Node // weights
	d0, d1, d2, d3     float64 // dropout probabilities

	out *G.Node
}

func newConvNet(g *G.ExprGraph) *convnet {
	w0 := G.NewTensor(g, dt, 4, G.WithShape(32, 1, 3, 3), G.WithName("w0"), G.WithInit(G.GlorotN(1.0)))
	w1 := G.NewTensor(g, dt, 4, G.WithShape(64, 32, 3, 3), G.WithName("w1"), G.WithInit(G.GlorotN(1.0)))
	w2 := G.NewTensor(g, dt, 4, G.WithShape(128, 64, 3, 3), G.WithName("w2"), G.WithInit(G.GlorotN(1.0)))
	w3 := G.NewMatrix(g, dt, G.WithShape(128*3*3, 625), G.WithName("w3"), G.WithInit(G.GlorotN(1.0)))
	w4 := G.NewMatrix(g, dt, G.WithShape(625, 10), G.WithName("w4"), G.WithInit(G.GlorotN(1.0)))

	return &convnet{
		g:  g,
		w0: w0,
		w1: w1,
		w2: w2,
		w3: w3,
		w4: w4,

		// The dropout probabilities are fixed to idiomatic values
		d0: 0.2,
		d1: 0.2,
		d2: 0.2,
		d3: 0.55,
	}

}

// Define the network by adding a method to the convnet structure
// we convolve with stride = (1, 1) and padding = (1, 1)
// which is bog standard convolution for convnet
// TODO: wrap up the layers within a layer struct type and perform per-layer activations
func (m *convnet) fwd(x *G.Node) (err error) {
	var c0, c1, c2, fc *G.Node
	var a0, a1, a2, a3 *G.Node
	var p0, p1, p2 *G.Node
	var l0, l1, l2, l3 *G.Node

	// Layer 0:
	if c0, err = G.Conv2d(x, m.w0, tensor.Shape{3, 3}, []int{1, 1}, []int{1, 1}, []int{1, 1}); err != nil {
		return errors.Wrap(err, "Layer 0 Convolution failed")
	}
	if a0, err = G.Rectify(c0); err != nil {
		return errors.Wrap(err, "Layer 0 Activation failed")
	}
	if p0, err = G.MaxPool2D(a0, tensor.Shape{2, 2}, []int{0, 0}, []int{2, 2}); err != nil {
		return errors.Wrap(err, "Layer 0 Maxpooling failed")
	}
	log.Printf("p0 shape %v", p0.Shape())

	if l0, err = G.Dropout(p0, m.d0); err != nil {
		return errors.Wrap(err, "Unable to apply a dropout")
	}

	// Layer 1:
	if c1, err = G.Conv2d(l0, m.w1, tensor.Shape{3, 3}, []int{1, 1}, []int{1, 1}, []int{1, 1}); err != nil {
		return errors.Wrap(err, "Layer 1 Convolution failed")
	}
	if a1, err = G.Rectify(c1); err != nil {
		return errors.Wrap(err, "Layer 1 Activation failed")
	}
	if p1, err = G.MaxPool2D(a1, tensor.Shape{2, 2}, []int{0, 0}, []int{2, 2}); err != nil {
		return errors.Wrap(err, "Layer 1 Maxpooling failed")
	}

	if l1, err = G.Dropout(p1, m.d1); err != nil {
		return errors.Wrap(err, "Unable to apply a dropout to layer 1")
	}

	// Layer 2:
	if c2, err = G.Conv2d(l1, m.w2, tensor.Shape{3, 3}, []int{1, 1}, []int{1, 1}, []int{1, 1}); err != nil {
		return errors.Wrap(err, "Layer 2 Convolution failed")
	}
	if a2, err = G.Rectify(c2); err != nil {
		return errors.Wrap(err, "Layer 2 Activation failed")
	}
	if p2, err = G.MaxPool2D(a2, tensor.Shape{2, 2}, []int{0, 0}, []int{2, 2}); err != nil {
		return errors.Wrap(err, "Layer 2 Maxpooling failed")
	}

	log.Printf("p2 shape %v", p2.Shape())

	var r2 *G.Node
	b, c, h, w := p2.Shape()[0], p2.Shape()[1], p2.Shape()[2], p2.Shape()[3]
	if r2, err = G.Reshape(p2, tensor.Shape{b, c * h * w}); err != nil {
		return errors.Wrap(err, "Unable to reshape layer 2")
	}
	log.Printf("r2 shape %v", r2.Shape())

	if l2, err = G.Dropout(r2, m.d2); err != nil {
		return errors.Wrap(err, "Unable to apply a dropout to layer 2")
	}

	ioutil.WriteFile("tmp.dot", []byte(m.g.ToDot()), 0644)

	// Layer 3
	if fc, err = G.Mul(l2, m.w3); err != nil {
		return errors.Wrapf(err, "Unable to multiply l2 and w3")
	}
	if a3, err = G.Rectify(fc); err != nil {
		return errors.Wrapf(err, "Unable to activate fc")
	}
	if l3, err = G.Dropout(a3, m.d3); err != nil {
		return errors.Wrapf(err, "Unable to apply a dropout on layer 3")
	}

	// Output decode
	var out *G.Node
	if out, err = G.Mul(l3, m.w4); err != nil {
		return errors.Wrapf(err, "Unable to multiply l3 and w4")
	}

	m.out, err = G.SoftMax(out)

	return
}

func (m *convnet) learnables() G.Nodes {
	return G.Nodes{m.w0, m.w1, m.w2, m.w3, m.w4}
}

func (m *convnet) save() {

}

func main() {

	flag.Parse()
	parseDtype()
	rand.Seed(1337)

	// intercept Ctrl + C
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	doneChan := make(chan bool, 1)

	var inputs, targets tensor.Tensor
	var err error

	go func() {
		log.Println(http.ListenAndServe("localhost:6060", nil))
	}()

	trainOn := *dataset

	if inputs, targets, err = mnist.Load(trainOn, loc, dt); err != nil {
		log.Fatal(err)
	}

	// The data is in (numExamples, 784)
	// In order to use convnet, we need to massage the data
	// into this format(batchsize, numberOfChannels, height, width)

	// This translates into (numExamples, 1, 28, 28)

	// This is because the convolution operators actually understand height, width
	// 1 indicates that there is only on channel (MNIST data is black and white)
	numExamples := inputs.Shape()[0]
	bs := *batchsize
	// TODO: check bs not 0

	if err = inputs.Reshape(numExamples, 1, 28, 28); err != nil {
		log.Fatal(err)
	}

	g := G.NewGraph()
	x := G.NewTensor(g, dt, 4, G.WithShape(bs, 1, 28, 28), G.WithName("x"))
	y := G.NewMatrix(g, dt, G.WithShape(bs, 10), G.WithName("y"))
	m := newConvNet(g)

	if err = m.fwd(x); err != nil {
		log.Fatalf("%v+", err)
	}

	losses := G.Must(G.Log(G.Must(G.HadamardProd(m.out, y))))
	cost := G.Must(G.Mean(losses))
	cost = G.Must(G.Neg(cost))

	// we're going to track costs
	var costVal G.Value
	G.Read(cost, &costVal)

	prog, logMap, _ := G.Compile(g)
	log.Printf("%v", prog)

	vm := G.NewTapeMachine(g, G.WithPrecompiled(prog, logMap), G.BindDualValues(m.learnables()...))
	solver := G.NewRMSPropSolver(G.WithBatchSize(float64(bs)))
	defer vm.Close()

	var profiling bool
	if *cpuprofile != "" {
		f, err := os.Create(*cpuprofile)
		if err != nil {
			log.Fatal(err)
		}
		profiling = true

		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}

	go cleanup(sigChan, doneChan, profiling)

	batches := numExamples / bs
	log.Printf("Batches %d", batches)
	bar := bp.New(batches)
	bar.SetRefreshRate(time.Second)
	bar.SetMaxWidth(80)

	for i := 0; i < *epochs; i++ {
		bar.Prefix(fmt.Sprintf("Epoch %d", i))
		bar.Set(0)
		bar.Start()

		for b := 0; b < batches; b++ {
			start := b * bs
			end := start + bs
			if start >= numExamples {
				break
			}
			if end > numExamples {
				end = numExamples
			}
			var xVal, yVal tensor.Tensor
			if xVal, err = inputs.Slice(sli{start, end}); err != nil {
				log.Fatal("Unable to slice x")
			}

			if yVal, err = targets.Slice(sli{start, end}); err != nil {
				log.Fatal("Unable to slice y")
			}

			if err = xVal.(*tensor.Dense).Reshape(bs, 1, 28, 28); err != nil {
				log.Fatalf("Unable to reshape %v", err)
			}

			G.Let(x, xVal)
			G.Let(y, yVal)
			if err = vm.RunAll(); err != nil {
				log.Fatalf("Failed at epoch %d: %v", i, err)
			}

			solver.Step(G.NodesToValueGrads(m.learnables()))
			vm.Reset()
			bar.Increment()
		}
		log.Printf("Epoch %d | cost %v", i, costVal)
	}

}

func cleanup(sigChan chan os.Signal, doneChan chan bool, profiling bool) {
	select {
	case <-sigChan:
		log.Println("Emergency Exit!")
		if profiling {
			log.Println("Stop profiling")
			pprof.StopCPUProfile()
		}
		os.Exit(1)
	case <-doneChan:
		return
	}
}

func handlePprof(sigChan chan os.Signal, doneChan chan bool) {
	var profiling bool
	if *cpuprofile != "" {
		f, err := os.Create(*cpuprofile)
		if err != nil {
			log.Fatal(err)
		}
		profiling = true
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}
	go cleanup(sigChan, doneChan, profiling)
}