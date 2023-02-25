load("@io_bazel_rules_go//go:def.bzl", "go_library")

go_library(
    name = "app",
    srcs = ["app.go"],
    importpath = "github.com/luluz66/review_bot/app",
    visibility = ["//visibility:public"],
    deps = [
        "@com_github_bradleyfalzon_ghinstallation_v2//:ghinstallation",
        "@com_github_go_git_go_git_v5//:go-git",
        "@com_github_go_git_go_git_v5//plumbing",
        "@com_github_go_git_go_git_v5//plumbing/object",
        "@com_github_google_go_github_v43//github",
    ],
)
