.PHONY: run vet build-go stop build up down rebuild logs reset cleanup prune-images

IMAGE_NAME := telegram-ogn-tracker
SERVICE := ogn-tracker

run: vet
	go run ./cmd/bot

vet:
	go vet ./...

build-go:
	go build -o ogn-go-bot ./cmd/bot

stop:
	docker stop $(SERVICE) || true
	docker rm $(SERVICE) || true

build:
	docker-compose build --no-cache --force-rm
	$(MAKE) prune-images

up:
	docker-compose up -d

down:
	docker-compose down

rebuild: down build up

prune-images:
	docker image prune -f

logs:
	docker logs -f $(SERVICE)

reset:
	rm -rf data/* logs/*

cleanup:
	docker stack rm $(IMAGE_NAME) || true
	docker-compose down || true
	docker rm -f $(shell docker ps -aq) || true
	docker rmi $(IMAGE_NAME) $(IMAGE_NAME)-$(SERVICE) || true
	docker image prune -f
	docker network rm $(IMAGE_NAME)_default || true
