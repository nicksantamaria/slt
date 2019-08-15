FROM previousnext/golang:1.12
ADD . /go/src/github.com/nicksantamaria/slt
WORKDIR /go/src/github.com/nicksantamaria/slt
RUN go install github.com/mitchellh/gox
RUN gox -os='linux' \
    	    -arch='amd64' \
    	    -output='bin/{{.OS}}/{{.Arch}}/slt' \
    	    github.com/nicksantamaria/slt

FROM debian:latest
COPY --from=0 /go/src/github.com/nicksantamaria/slt/bin/linux/amd64/ /usr/local/bin/
ENTRYPOINT ["/usr/local/bin/slt"]
EXPOSE 443