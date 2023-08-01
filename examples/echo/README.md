# Echo

This directory contains a Service Weaver web application that echos the request
string back to the caller.

This application exists primarily to test the correctness of the Kube deployer.

## How to run?

To run this application inside a Kubernetes cluster, run `weaver kube deploy`.

```console
$ weaver kube deploy weaver.toml    # Generate YAML file
$ kubectl apply -f <filename>       # Deploy YAML file
```

## How to interact with the application?

Get the IP address of the listener service on your kubernetes cluster. Then run:

```console
$ curl http://<IPAddress>?s=foo
foo
```