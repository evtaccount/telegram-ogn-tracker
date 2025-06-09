.PHONY: install lint run stop

install:
	pip install -r requirements.txt

lint:
	python3 -m py_compile bot.py

run:
	python bot.py

run-go:
	go run main.go

stop:
	docker stop ogn-tracker || true
	docker rm ogn-tracker || true
