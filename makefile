# Makefile для управления ботом в Docker

IMAGE_NAME := telegram-ogn-tracker
SERVICE := ogn-tracker

build:
	docker-compose build --no-cache

up:
	docker-compose up -d

down:
	docker-compose down

rebuild: down build up

logs:
	docker logs -f $(SERVICE)

cleanup:
	docker stack rm telegram-ogn-tracker || true
	docker-compose down || true
	docker rm -f $(shell docker ps -aq) || true
	docker rmi telegram-ogn-tracker telegram-ogn-tracker-ogn-tracker || true
	docker image prune -f
	docker network rm telegram-ogn-tracker_default || true
