FROM debian:bookworm-20240812

RUN apt update -y
RUN apt install -y build-essential ca-certificates
RUN update-ca-certificates

COPY ./entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

ENTRYPOINT ["/entrypoint.sh"]
