package(default_visibility = ["//visibility:public"])

licenses(["notice"])

load("@io_bazel_rules_go//go:def.bzl", "go_library")

go_library(
    name = "go_default_library",
    srcs = [
        "cert.go",
        "csr.go",
        "io.go",
        "pem.go",
    ],
    data = [
        "testdata/dontUseThisKey.pem",
    ],
    importmap = "github.com/dotmesh-io/dotmesh/vendor/k8s.io/client-go/util/cert",
    importpath = "k8s.io/client-go/util/cert",
    tags = ["automanaged"],
)
