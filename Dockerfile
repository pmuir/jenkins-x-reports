FROM scratch
EXPOSE 8080
ENTRYPOINT ["/jenkins-x-reports"]
COPY ./bin/ /