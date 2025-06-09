.PHONY: install lint run run-go stop build up down rebuild logs reset cleanup

IMAGE_NAME := telegram-ogn-tracker
SERVICE := ogn-tracker

install:
	pip install -r requirements.txt

lint:
	python3 -m py_compile bot.py

run:
	python bot.py

run-go:
	go run main.go

stop:
	docker stop $(SERVICE) || true
	docker rm $(SERVICE) || true

build:
	docker-compose build --no-cache

up:
	docker-compose up -d

down:
	docker-compose down

rebuild: down build up

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
