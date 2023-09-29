FROM golang:1.19-alpine AS build

WORKDIR /go/src/app
COPY ./ ./

RUN apk add build-base

RUN go get -d -v ./...
RUN go build -v -o injector . 

FROM golang:1.19-alpine

WORKDIR /go/src/app
COPY --from=build /go/src/app/ /go/src/app/
EXPOSE 8443
CMD ["./injector"]