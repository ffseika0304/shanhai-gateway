.PHONY: build test fmt vet clean docker

# build 出可执行文件到仓库根（配合 .gitignore 的 /shg）。
build:
	go build -o shg ./cmd/shg

test:
	go test ./... -race -count=1

fmt:
	gofmt -w internal/ cmd/ web/

vet:
	go vet ./...

clean:
	rm -f shg

# 镜像构建（见 deploy/ 与 README 八）：真实配置不进镜像，运行时挂载 runtime/。
docker:
	docker build -t shg:0.5.0 -f deploy/Dockerfile .
