.PHONY: build-lambdas
build-lambdas:
	cd sweeper && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o bootstrap ./
	cd budget  && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o bootstrap ./

.PHONY: init
init: build-lambdas
	terraform init

.PHONY: plan
plan: build-lambdas
	terraform plan

.PHONY: apply
apply: build-lambdas
	terraform apply

.PHONY: destroy
destroy:
	terraform destroy

.PHONY: fmt
fmt:
	terraform fmt -recursive
