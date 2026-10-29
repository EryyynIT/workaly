#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

# Keep compiler and plugins identical locally and in CI, including generated headers.
case "$(uname -s)-$(uname -m)" in
  Linux-x86_64) platform=linux-x86_64 ;;
  Linux-aarch64) platform=linux-aarch_64 ;;
  *) echo "Code generation currently supports Linux x86_64/aarch64" >&2; exit 1 ;;
esac

root="$PWD/.tools"
mkdir -p "$root/protoc" "$root/bin"
if [[ ! -x "$root/protoc/bin/protoc" ]]; then
  curl --fail --location --retry 3 \
    "https://github.com/protocolbuffers/protobuf/releases/download/v31.1/protoc-31.1-$platform.zip" \
    -o "$root/protoc.zip"
  unzip -qo "$root/protoc.zip" -d "$root/protoc"
fi
export GOBIN="$root/bin"
go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.6
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1
PATH="$GOBIN:$PATH" "$root/protoc/bin/protoc" -I contracts \
  --go_out=auth-service/pb/userpb --go_opt=paths=source_relative \
  --go-grpc_out=auth-service/pb/userpb --go-grpc_opt=paths=source_relative \
  contracts/user.proto

# protoc does not create output directories.
mkdir -p auth-service/pb/jobpb job-service/pb/jobpb auth-service/pb/taskpb task-service/pb/taskpb
PATH="$GOBIN:$PATH" "$root/protoc/bin/protoc" -I contracts \
  --go_out=auth-service/pb/jobpb --go_opt=paths=source_relative \
  --go-grpc_out=auth-service/pb/jobpb --go-grpc_opt=paths=source_relative \
  contracts/job.proto

# The job service owns the same contract in Go. A second copy inside its own
# module keeps its image self-contained (no auth-service sources in the build);
# the M override replaces go_package without touching the .proto.
PATH="$GOBIN:$PATH" "$root/protoc/bin/protoc" -I contracts \
  --go_out=job-service/pb/jobpb \
  --go_opt=paths=source_relative,Mcontracts/job.proto=github.com/EryyynIT/workaly/job-service/pb/jobpb \
  --go-grpc_out=job-service/pb/jobpb \
  --go-grpc_opt=paths=source_relative,Mcontracts/job.proto=github.com/EryyynIT/workaly/job-service/pb/jobpb \
  contracts/job.proto

# The task contract mirrors job.proto for the order domain. Same two-copy
# layout: the edge module and the owning service module each carry the pb.
PATH="$GOBIN:$PATH" "$root/protoc/bin/protoc" -I contracts \
  --go_out=auth-service/pb/taskpb --go_opt=paths=source_relative \
  --go-grpc_out=auth-service/pb/taskpb --go-grpc_opt=paths=source_relative \
  contracts/task.proto
PATH="$GOBIN:$PATH" "$root/protoc/bin/protoc" -I contracts \
  --go_out=task-service/pb/taskpb \
  --go_opt=paths=source_relative,Mcontracts/task.proto=github.com/EryyynIT/workaly/task-service/pb/taskpb \
  --go-grpc_out=task-service/pb/taskpb \
  --go-grpc_opt=paths=source_relative,Mcontracts/task.proto=github.com/EryyynIT/workaly/task-service/pb/taskpb \
  contracts/task.proto
