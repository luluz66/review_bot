load("@io_bazel_rules_go//go:def.bzl", "go_binary", "go_library")

# Prefer generated BUILD files to be called BUILD over BUILD.bazel
# gazelle:build_file_name BUILD,BUILD.bazel
# gazelle:prefix github.com/luluz66/review_bot
load("@bazel_gazelle//:def.bzl", "gazelle")

gazelle(name = "gazelle")

gazelle(
    name = "gazelle-update-repos",
    args = [
        "-from_file=go.mod",
        "-to_macro=deps.bzl%go_dependencies",
        "-prune",
    ],
    command = "update-repos",
)

go_library(
    name = "review_bot_lib",
    srcs = ["main.go"],
    importpath = "github.com/luluz66/review_bot",
    visibility = ["//visibility:private"],
    deps = ["//app"],
)

go_binary(
    name = "review_bot",
    embed = [":review_bot_lib"],
    visibility = ["//visibility:public"],
)
