package main

import (
    "bufio"
    "fmt"
    "os"
    "time"
    "strings"
    "math/rand"
    
    "github.com/miekg/dns"
    flag 	"github.com/spf13/pflag"
)

const workerExit = "~"
const workerNotifyExit = "!~"
const separator = " ----------------------------------------------------------------"

const letterBytes = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

type result struct {
    dns string
    rtt float64
}

type resultStats struct {
    dns         string
    rtt         float64
    succ        int
    fail        int
    filtered    bool
}

type testInfo struct {
    domain string
    dns string
    rtt float64
}

var (
	numWorkersArg	    int
	numTestsArg		    int
    timefilterArg		int
    errorfilterArg		int
    ratefilterArg		int
    
    saveJustDNSArg		bool
    
	inArg		        string
	outArg            	string
	testDomainArg     	string
)

func randStringBytes(n int) string {
    b := make([]byte, n)
    for i := range b {
        b[i] = letterBytes[rand.Intn(len(letterBytes))]
    }
    return string(b)
}

func getDNSList() ([]string, error) {
    file, err := os.Open(inArg)
    if err != nil {
        return nil, fmt.Errorf("[!!!] Can't open file: %s", inArg)
    }
    defer file.Close()

    var lines []string
    scanner := bufio.NewScanner(file)
    for scanner.Scan() {
        lines = append(lines, scanner.Text())
    }

    return lines, nil
}

func printHeader() {
    fmt.Println(`
           _            __          _
          | |          / _|        | |
        __| |_ __  ___| |_ __ _ ___| |_ ___ _ __
       / _' | '_ \/ __|  _/ _' / __| __/ _ \ '__|
      | (_| | | | \__ \ || (_| \__ \ ||  __/ |
       \__,_|_| |_|___/_| \__,_|___/\__\___|_|

    `)

    fmt.Println(separator)
    fmt.Printf("| %7d threads     | domain  : %30s |\n", numWorkersArg, testDomainArg)
    fmt.Printf("| %7d tests       | in file : %30s |\n", numTestsArg, inArg)
    fmt.Println(separator)
    fmt.Println("| status |                ip | avg milsec | Rate |  Succ |  Fail |")
    fmt.Println(separator)
}

func workerResolverChecker(dc chan *testInfo, receiver chan *testInfo, baseDomain string) {
    for {
        test, ok := <-dc
        if !ok || test.dns == workerExit {
            break
        }

        if test.dns == workerNotifyExit {
            receiver<-nil
            break
        }

        c := dns.Client{}
        m := dns.Msg{}

        m.SetQuestion(test.domain + ".", dns.TypeA)
        r, rtt, err := c.Exchange(&m, test.dns + ":53")

        // make sure the server responds and returns no entry
        if err == nil && r != nil && r.Rcode == dns.RcodeNameError {
            test.rtt = float64(rtt/time.Millisecond)
        }
        receiver<-test
    }
}


func receiverService(rcv chan *testInfo, done chan bool) {
    
    var w *bufio.Writer

    results := make(map[string]*resultStats)

    defer func() { done<-true }() // close the channel once done

    if(len(outArg) > 0){
                
        os.Remove(outArg)
        file, err := os.OpenFile(outArg, os.O_WRONLY | os.O_CREATE, 0644)
        if err != nil {
            fmt.Println("[!] Can't open file to save: ", outArg)
            return
        }

        defer file.Close()

        w = bufio.NewWriter(file)
    }

    for {

        result, ok := <-rcv
        if !ok || result == nil{
            break
        }

        _, prs := results[result.dns]
        if !prs {
            results[result.dns] = new(resultStats)
            results[result.dns].dns = result.dns
        }

        cur := results[result.dns]

        if result.rtt == -1 {
            cur.fail++
        } else {
            cur.rtt += result.rtt
            cur.succ++
        }

        if cur.succ + cur.fail == numTestsArg {
            if cur.rtt != 0 {
                cur.rtt = cur.rtt / float64(cur.succ)
            }
            succP := cur.succ*100/numTestsArg
            
            if timefilterArg > 0 && int(cur.rtt) > timefilterArg{
                //fmt.Printf("%v filtered by time!: %v  || %v || %v \n", cur.dns, timefilterArg, int(cur.rtt), cur.rtt)
                cur.filtered = true
            }

            if errorfilterArg > 0 && cur.fail > errorfilterArg{
                //fmt.Printf("%v filtered by error!: %v || %v \n", cur.dns, errorfilterArg, cur.fail)
                cur.filtered = true
            }

            if ratefilterArg > 0 && succP > ratefilterArg{
                //fmt.Printf("%v filtered by ratelimit!: %v || %v\n", cur.dns, ratefilterArg, succP)
                cur.filtered = true
            }

            if(cur.filtered){
                fmt.Printf("| FILTER | %17s | %10v | %3d%% | %5d | %5d |\n", cur.dns, int(cur.rtt), succP, cur.succ, cur.fail)
            }else{                
                fmt.Printf("| OK     | %17s | %10v | %3d%% | %5d | %5d |\n", cur.dns, int(cur.rtt), succP, cur.succ, cur.fail)
            }
                
            var s string
            if(saveJustDNSArg){
                s = fmt.Sprintf("%s\n", cur.dns)
            } else{
                s = fmt.Sprintf("%s,%v,%d,%d,%d\n", cur.dns, int(cur.rtt), succP, cur.succ, cur.fail)
            }
                            
            if(len(outArg) > 0){
                if(!cur.filtered) {
                    if _, err := w.WriteString(s); err != nil {
                        fmt.Println(err)
                        os.Exit(1)
                    }
                }
            }            
        }
    }
    if(len(outArg) > 0){
        if err := w.Flush(); err != nil {
        fmt.Println(err)
        os.Exit(1)
        }
    }

    fmt.Println(separator)
}

func distributorService(){

    rand.Seed(time.Now().UnixNano())

    printHeader()

    resolvers, err := getDNSList()
    if err != nil {
        fmt.Println(err)
        return
    }

    // pregenerate test cases
    var domains []string
    for i := 0; i < numTestsArg; i++ {
        domains = append(domains, strings.Join([]string{randStringBytes(8), ".", testDomainArg}, ""))
    }

    dc := make(chan *testInfo, 1000)
    receiver := make(chan *testInfo, 250)

    rcvDone := make(chan bool)

    go receiverService(receiver, rcvDone)

    for i := 0; i < numWorkersArg; i++ {
        go workerResolverChecker(dc, receiver, testDomainArg)
    }

    for i := 0; i < numTestsArg; i++ {
        for _, dns := range resolvers {
            test := new(testInfo)
            test.dns = dns
            test.domain = domains[i]
            test.rtt = -1
            dc<-test
        }
    }

    for i := 0; i < numWorkersArg; i++ {
        test := new(testInfo)
        if i+1 == numWorkersArg { // last worker notifies receiver
            test.dns = workerNotifyExit
        } else {
            test.dns = workerExit
        }
        dc<-test
    }

    <-rcvDone
}

func main() {

    flag.StringVar(&inArg, "in", "", "DNS servers list")
    flag.StringVar(&outArg, "out", "", "Output file to save the results to")
    flag.StringVar(&testDomainArg, "domain", "example.com", "Domain name to test against")
    
    flag.IntVar(&numWorkersArg, "workers", 10, "Number of workers")
	flag.IntVar(&numTestsArg, "tests", 10, "Number of workers")
    flag.IntVar(&timefilterArg, "filter-time", 0, "Number of workers")
    flag.IntVar(&errorfilterArg, "filter-errors", 0, "Number of workers")
    flag.IntVar(&ratefilterArg, "filter-rate", 0, "Number of workers")
    
    flag.BoolVar(&saveJustDNSArg, "save-dns", false, "Save just the DNS hostname")

    flag.Parse();

    if numWorkersArg < 1 || numWorkersArg > 100 {
        fmt.Fprintf(os.Stderr, "[!] Invalid number of workers: %d\n", numWorkersArg)
        return
    }

    if numTestsArg < 1  || numTestsArg > 500{
        fmt.Fprintf(os.Stderr, "[!] Invalid number of tests: %d\n", numTestsArg)
        return
    }

    distributorService()
}
