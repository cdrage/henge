# Henge
[![Build Status](https://travis-ci.org/redhat-developer/henge.svg?branch=master)](https://travis-ci.org/redhat-developer/henge)

Convert multi-container application defined using Docker Compose to Kubernetes and/or Openshift.

Project goals:
- The tool should be intelligent and interactive enough to ask the right questions when it is trying to convert and assume sane defaults
- The output file generated by the tool should be consumable by `kubectl`/`oc` and should be able to handle various Docker Compose specifics like services, networking and map them to Kubernetes/OpenShift world.
- The tool can be used as a library or service with another front end e.g. oc, kubectl, a web app or an IDE.



## Usage

### Examples

To convert the docker-compose.yml file in the current directory to openshift's artifacts
```
henge openshift
```

To convert the file of your choice to kubernetes's artifacts.
```
henge kubernetes -f foo.yml
```

To convert docker-compose.yml file in current directory and also ask questions interactively.

```
henge openshift -i
```

To provide multiple file for conversion
```
henge kubernetes -f foo.yml,bar.yml,docker-compose.yml
```





## Developing and building from source

### Setting up GOPATH

Follow instructions [here](https://golang.org/doc/code.html#GOPATH) to setup GO developer environment.


### Getting sources

If you are building upstream code
```bash
go get github.com/redhat-developer/henge
cd $GOPATH/src/github.com/redhat-developer/henge/
```

If you developing and using your own fork
```bash
mkdir -p $GOPATH/src/github.com/redhat-developer
cd $GOPATH/src/github.com/redhat-developer
git clone https://github.com/<forkid>/henge
cd henge/
git remote add upstream https://github.com/redhat-developer/henge
```

### Build
Check your Go version `go version`

#### using Go v1.6
```
go build henge.go
```

#### using Go v1.5
```
GO15VENDOREXPERIMENT=1 go build henge.go
```

### Debug
You can run henge with verbose logging by adding `-v 5` option
```
./henge --loglevel=5 openshift -f docker-compose.yml
```
