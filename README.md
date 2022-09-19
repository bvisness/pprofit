# pprofit!

1. Ship slow code
2. ?
3. pprofit!

pprofit is a dashboard to streamline `pprof` profiling of Go apps. It makes it easy to access all the built-in profile types, and to save and access them.

## How to use

Install it:

```
go install github.com/bvisness/pprofit@latest
```

Then pprof it! Simply run `pprofit`, enter the `pprof` URL of your Go application, select a profile type, and click Capture.

Requires the use of [`net/http/pprof`](https://pkg.go.dev/net/http/pprof) in your Go application.
