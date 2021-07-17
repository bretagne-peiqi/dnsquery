package main

import (
	"context"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/lixiangzhong/dnsutil"
)

type Request struct {
	ID   int
	done chan struct{}

	do     func(ctx context.Context, host string) (*http.Response, error)
	result *http.Response
}

func NewRequest(id int, do func(ctx context.Context, host string) (*http.Response, error)) *Request {
	return &Request{
		ID:   id,
		done: make(chan struct{}),
		do:   do,
	}
}

func (r *Request) Do(ctx context.Context, host string) (err error) {
	result, err := r.do(ctx, host)
	if err != nil {
		return err
	}
	r.result = result
	close(r.done)
	return nil
}

func (r *Request) Wait() {
	<-r.done
}

type DomainIPs struct {
	sync.Mutex

	LookupAddr     func(ctx context.Context, domain string) ([]string, error)
	Domain         string
	WorkerNumPerIp int

	requests chan *Request
	hosts    map[string]struct{}
	workers  map[string]int
}

func NewDomainIPs(domain string, workerNumPerIp int, lookupAddr func(ctx context.Context, domain string) ([]string, error)) *DomainIPs {
	d := &DomainIPs{
		LookupAddr:     lookupAddr,
		Domain:         domain,
		WorkerNumPerIp: workerNumPerIp,

		requests: make(chan *Request, 100),
		hosts:    make(map[string]struct{}),
		workers:  make(map[string]int),
	}
	if d.LookupAddr == nil {
		d.LookupAddr = func(ctx context.Context, domain string) (strings []string, err error) {
			var dig dnsutil.Dig
			as, err := dig.A("ebay.com")
			if err != nil {
				return nil, err
			}
			hosts := make([]string, len(as))
			for idx, d := range as {
				hosts[idx] = d.A.String()
			}
			return hosts, nil
		}
	}

	go func() {
		for {
			if err := d.query(context.Background()); err == nil {
				time.Sleep(time.Second * 10)
			} else {
				time.Sleep(time.Second * 3)
			}
		}
	}()

	return d
}

func (d *DomainIPs) query(ctx context.Context) error {
	hosts, err := d.LookupAddr(ctx, d.Domain)
	if err != nil {
		return err
	}

	var newHosts = make(map[string]int)

	d.Lock()
	d.hosts = make(map[string]struct{})
	for _, host := range hosts {
		d.hosts[host] = struct{}{}
		newHosts[host] = d.WorkerNumPerIp - d.workers[host]
		d.workers[host] = d.WorkerNumPerIp
	}
	d.Unlock()

	for newHost, num := range newHosts {
		for i := 0; i < num; i++ {
			go func(host string) {
				log.Printf("%s created", host)
				var lastDownTime time.Time
				for req := range d.requests {
					//log.Printf("handling request: %d", req.ID)
					ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
					if nil != req.Do(ctx, host) {
						go func() {
							d.requests <- req
						}()
						lastDownTime = time.Now()
						_ = d.query(context.Background())
					}
					cancel()

					for {
						d.Lock()
						if _, ok := d.hosts[host]; !ok {
							if d.workers[host]--; d.workers[host] == 0 {
								delete(d.workers, host)
							}
							d.Unlock()

							log.Printf("%s exited", host)
							return
						}
						d.Unlock()

						if time.Since(lastDownTime) > time.Minute {
							break
						}
						time.Sleep(time.Second * 5)
					}
				}
			}(newHost)
		}
	}
	return nil
}

func Req(domain string, requests []*Request) {
	d := NewDomainIPs(domain, 4, nil)
	for _, req := range requests {
		d.requests <- req
	}
	for _, req := range requests {
		req.Wait()
	}
}

func ReqURLs(domain string, urls []string) []*http.Response {
	var requests = make([]*Request, len(urls))
	for i, url := range urls {
		requests[i] = NewRequest(i, func(ctx context.Context, host string) (response *http.Response, err error) {
			client := &http.Client{
				Timeout: time.Second * 5,
			}
			if strings.HasPrefix(url, "/") {
				url = url[1:]
			}
			req, err := http.NewRequest("GET", fmt.Sprintf("http://%s/%s", host, url), nil)
			if err != nil {
				return nil, err
			}
			resp, err := client.Do(req)
			if err != nil {
				return nil, err
			}
			if 200 <= resp.StatusCode && resp.StatusCode <= 500 {
				return resp, nil
			}
			return nil, fmt.Errorf("code %d not in [200-500], resp: %v", resp.StatusCode, resp)
		})
	}
	Req(domain, requests)
	resps := make([]*http.Response, len(urls))
	for i, req := range requests {
		if req.result == nil {
			panic("req.result == nil")
		}
		resps[i] = req.result
	}
	return resps
}

func main() {
	urls := make([]string, 100)
	for i := 0; i < 100; i++ {
		urls[i] = "itm/313208030296"
	}
	resps := ReqURLs("www.ebay.com", urls)
	for _, resp := range resps {
		log.Printf("req: %s", resp.Request.URL)
		if resp.StatusCode == http.StatusOK {
			bodyBytes, err := ioutil.ReadAll(resp.Body)
			if err != nil {
				log.Fatal(err)
			}
			bodyString := string(bodyBytes)
			log.Printf(bodyString)
		} else {
			log.Printf("Not OK, code: %d", resp.StatusCode)
		}
		log.Printf("---------------------------------------------")
	}
}
