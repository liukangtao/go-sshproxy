基于ssh动态端口代理，使用go语言实现了socks5和http代理工作。
工作时需要一个远端联网并开启ssh服务的服务器，在本地启用本代理后会在本地启动一个代理解析服务，可解析socks5或http请求，并将请求通过ssh转发给远端服务器进行代请求，并将远端服务器代请求的结果返回给请求方。
在请求过程中会同时进行本地代请求和远端服务器代请求，选取最快的进行后续请求，不需要设置代理名单即可区分国内请求和国外请求。
