package main

import "net/http"

func main() {
	mux := http.NewServeMux()
	fileServer1 := http.FileServer(http.Dir("."))
	fileServer2 := http.FileServer(http.Dir("./assets"))
	mux.Handle("/", fileServer1)
	mux.Handle("/assets/", http.StripPrefix("/assets/", fileServer2))
	server := &http.Server{
		Addr:    ":8080",
		Handler: mux,
	}
	server.ListenAndServe()
}
