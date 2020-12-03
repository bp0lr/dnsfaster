## dnsfaster


### why?

I have started working on an app to brute force subdomains using mutations, permutations and alterations.
This will test millions of combinations again a list of DNS servers. I need a tool to keep my DNS servers list clean and fast.
I didn't found anything to fit what I want at 100%, so I take dnsfaster and I change some things.


### what dnsfaster does

This tool will test (many times) your DNS list again a test domain to validate responses.
This is useful if you want to keep your DNS list under an error rate or speed limit.


### Install

Install is quick and clean
```
go get github.com/bp0lr/dnsfaster
```


### examples
```
./dnsfaster --domain example.com --in "dnslist.txt" --out "resolvers.txt" --tests 1000 --workers 50 --filter-time 400 --filter-errors 50 --filter-rate 90 --save-dns
```

this will run dnsfaster again the dns servers in dnslist.txt, testing 1000 times on each server using 50 workers, and filter out the domains with response time average > 400ms, errors > 50 and sucess rate < 90%.
Use --save-dns if you want just the dns ip saved in the result file.


### Why dnsfaster was not forked

the original author has moved out his repo to gitlab.
You can visit his version at
```
https://gitlab.com/jules.rigaudie/dnsfaster
```
thanks @jules.rigaudie for your work. :)
