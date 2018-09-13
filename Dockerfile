FROM scratch
EXPOSE 8080
EXPOSE 8081
ENTRYPOINT ["/jenkins-x-reports"]
COPY ./bin/ /